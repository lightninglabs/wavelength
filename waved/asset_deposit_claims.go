package waved

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	tapsdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/chainsource"
	"github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/round"
	"github.com/lightninglabs/wavelength/tapassets"
	"github.com/lightninglabs/wavelength/txconfirm"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	// assetClaimJournalPrefix keys deposit claims in the claim journal.
	assetClaimJournalPrefix = "deposit/"

	// assetClaimSubscriberName names the actor that wakes the deposit
	// watcher when txconfirm reports on a claim.
	assetClaimSubscriberName = "asset-deposit-claims"

	// claimFinalityDepth is the depth at which a claim is final. txconfirm
	// stops following a transaction through reorgs at the same depth.
	claimFinalityDepth = chainsource.DefaultFinalityDepth
)

// assetDepositClaimStore persists deposit claims.
type assetDepositClaimStore interface {
	CreateAssetDepositClaim(context.Context, string,
		chainhash.Hash) (*db.AssetDepositClaim, error)

	GetAssetDepositClaim(context.Context,
		string) (*db.AssetDepositClaim, error)

	ListAssetDepositClaimsByStatus(context.Context,
		db.AssetDepositClaimStatus) ([]*db.AssetDepositClaim, error)

	MarkAssetDepositClaimed(context.Context, string) error
}

// assetClaimer claims a composed output back into the tapd wallet. Prepare
// signs the claim without recording it in tapd, and Claim records it.
type assetClaimer interface {
	Prepare(context.Context,
		*tapassets.ClaimRequest) (*tapassets.ClaimResult, error)

	Claim(context.Context,
		*tapassets.ClaimRequest) (*tapassets.ClaimResult, error)
}

// claimReserver reserves a deposit for a claim with the round actor, which
// refuses while a round holds the deposit.
type claimReserver func(context.Context, wire.OutPoint) error

// claimNotifications receives txconfirm notifications for deposit claims.
type claimNotifications = actor.TellOnlyRef[txconfirm.Notification]

// claimBroadcaster confirms a zero-fee claim with a CPFP child.
type claimBroadcaster func(context.Context, *wire.MsgTx, uint32) error

// Claim spends a confirmed deposit that no round holds through its owner's
// exit leaf, once the exit delay has passed, and returns the asset to the
// tapd wallet. A retry resumes the journaled claim.
func (d *assetDepositService) Claim(ctx context.Context, requestID string) (
	*db.AssetDeposit, error) {

	if requestID == "" {
		return nil, status.Error(
			codes.InvalidArgument, "request_id is required",
		)
	}
	if d.claims == nil || d.claimer == nil || d.broadcastClaim == nil ||
		d.reserveClaim == nil {
		return nil, status.Error(
			codes.FailedPrecondition,
			"asset deposit claims are not available",
		)
	}

	deposit, err := d.store.GetAssetDeposit(ctx, requestID)
	if errors.Is(err, db.ErrAssetDepositNotFound) {
		return nil, status.Errorf(codes.NotFound, "asset deposit %s "+
			"not found", requestID)
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "load asset "+
			"deposit: %v", err)
	}
	if deposit.Status != db.AssetDepositConfirmed ||
		deposit.Output == nil {
		return nil, status.Errorf(codes.FailedPrecondition, "asset "+
			"deposit %s is %s, not confirmed", requestID,
			deposit.Status)
	}

	claim, err := d.ClaimOf(ctx, requestID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "load asset deposit "+
			"claim: %v", err)
	}
	if claim != nil && claim.Status == db.AssetDepositClaimed {
		return deposit, nil
	}

	roundID, err := d.store.AssetDepositRound(ctx, requestID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "load asset deposit "+
			"round: %v", err)
	}
	if roundID != "" {
		return nil, status.Errorf(codes.FailedPrecondition, "asset "+
			"deposit %s is held by round %s", requestID, roundID)
	}
	if err := d.checkClaimable(ctx, deposit); err != nil {
		return nil, err
	}

	// The claim is recorded before tapd learns of it, so the watcher
	// resumes every claim tapd may hold after a restart.
	if claim == nil {
		if err := d.recordClaim(ctx, deposit); err != nil {
			return nil, err
		}
	}

	result, err := d.resumeClaim(ctx, deposit)
	if err != nil {
		return nil, err
	}

	return deposit, d.trackClaim(ctx, deposit, result.Tx)
}

// checkClaimable fails unless tapd credits a claimed asset and the deposit's
// exit leaf is open.
func (d *assetDepositService) checkClaimable(ctx context.Context,
	deposit *db.AssetDeposit) error {

	info, err := d.tapd.GetInfo(ctx)
	if err != nil || info == nil {
		return status.Errorf(codes.Unavailable, "read tapd info: %v",
			err)
	}
	supported, err := tapassets.TapdCanClaim(info.Version)
	if err != nil {
		return status.Errorf(codes.FailedPrecondition, "asset deposit "+
			"claims: %v", err)
	}
	if !supported {
		return status.Errorf(codes.FailedPrecondition, "tapd %s "+
			"cannot credit a claimed asset; claims require tapd "+
			"v0.9.0 or later", info.Version)
	}

	// The exit leaf's relative timelock lets the claim into the block
	// at confirmed_height + exit_delay at the earliest.
	mature := uint64(deposit.ConfirmedHeight) +
		uint64(deposit.Request.ExitDelay)
	if uint64(info.BlockHeight)+1 < mature {
		return status.Errorf(codes.FailedPrecondition, "asset deposit "+
			"%s can be claimed from height %d",
			deposit.Request.RequestID, mature)
	}

	return nil
}

// recordClaim signs a deposit's claim, reserves the deposit against round
// boarding, and records the claim.
func (d *assetDepositService) recordClaim(ctx context.Context,
	deposit *db.AssetDeposit) error {

	requestID := deposit.Request.RequestID
	request, err := depositClaimRequest(deposit)
	if err != nil {
		return status.Errorf(codes.Internal, "build asset deposit "+
			"claim: %v", err)
	}
	prepared, err := d.claimer.Prepare(ctx, request)
	if err != nil {
		return status.Errorf(codes.Internal, "prepare asset deposit "+
			"claim: %v", err)
	}

	err = d.reserveClaim(ctx, deposit.Output.Outpoint)
	if errors.Is(err, round.ErrBoardingInFlight) {
		return status.Errorf(codes.FailedPrecondition, "asset deposit "+
			"%s is in a round", requestID)
	}
	if err != nil {
		return status.Errorf(codes.Unavailable, "reserve asset "+
			"deposit %s: %v", requestID, err)
	}

	_, err = d.claims.CreateAssetDepositClaim(
		ctx, requestID, prepared.Tx.TxHash(),
	)
	if err != nil {
		return status.Errorf(codes.Internal, "record asset deposit "+
			"claim: %v", err)
	}

	return nil
}

// ClaimOf returns the claim of a deposit, or nil when it has none.
func (d *assetDepositService) ClaimOf(ctx context.Context, requestID string) (
	*db.AssetDepositClaim, error) {

	if d.claims == nil {
		return nil, nil
	}
	claim, err := d.claims.GetAssetDepositClaim(ctx, requestID)
	if errors.Is(err, db.ErrAssetDepositClaimNotFound) {
		return nil, nil
	}

	return claim, err
}

// resumeClaim signs and records the deposit's claim in tapd, or returns the
// journaled claim.
func (d *assetDepositService) resumeClaim(ctx context.Context,
	deposit *db.AssetDeposit) (*tapassets.ClaimResult, error) {

	request, err := depositClaimRequest(deposit)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "build asset "+
			"deposit claim: %v", err)
	}
	result, err := d.claimer.Claim(ctx, request)
	switch {
	case errors.Is(err, tapassets.ErrReconciliationRequired):
		return nil, status.Errorf(codes.Unavailable, "asset deposit "+
			"claim: %v", err)

	case err != nil:
		return nil, status.Errorf(codes.Internal, "asset deposit "+
			"claim: %v", err)
	}

	return result, nil
}

// trackClaim hands a recorded claim to txconfirm. A claim that txconfirm did
// not accept stays recorded, and the watcher retries it.
func (d *assetDepositService) trackClaim(ctx context.Context,
	deposit *db.AssetDeposit, tx *wire.MsgTx) error {

	requestID := deposit.Request.RequestID
	txid := tx.TxHash()
	err := d.broadcastClaim(ctx, tx, deposit.ConfirmedHeight)
	if err != nil {
		return status.Errorf(codes.Unavailable, "broadcast asset "+
			"deposit claim %s: %v", txid, err)
	}

	d.claimsMu.Lock()
	d.trackedClaims[txid] = struct{}{}
	d.claimsMu.Unlock()

	d.log.InfoS(ctx, "Asset deposit claim broadcast",
		slog.String("request_id", requestID),
		slog.String("claim_txid", txid.String()),
	)
	d.requestPoll()

	return nil
}

// confirmClaims marks the claims buried at the finality depth by tapd's chain
// tip, and hands every other pending claim to txconfirm again after a
// restart. txconfirm follows a claim through reorgs until that depth.
func (d *assetDepositService) confirmClaims(ctx context.Context) {
	if d.claims == nil || d.claimer == nil {
		return
	}

	claims, err := d.claims.ListAssetDepositClaimsByStatus(
		ctx, db.AssetDepositClaiming,
	)
	if err != nil {
		d.log.DebugS(ctx, "Unable to list asset deposit claims",
			slog.String("error", err.Error()),
		)

		return
	}

	if len(claims) == 0 {
		return
	}
	info, err := d.tapd.GetInfo(ctx)
	if err != nil || info == nil {
		d.log.DebugS(ctx, "Unable to read the tapd chain tip",
			btclog.Fmt("error", "%v", err),
		)

		return
	}

	for _, claim := range claims {
		final, err := d.claimFinal(
			ctx, claim.ClaimTxid, info.BlockHeight,
		)
		if err != nil {
			d.log.DebugS(ctx, "Unable to read asset deposit claim",
				slog.String("request_id", claim.RequestID),
				btclog.Fmt("error", "%v", err),
			)

			continue
		}
		if final {
			err := d.claims.MarkAssetDepositClaimed(
				ctx, claim.RequestID,
			)
			if err != nil {
				d.recordError(ctx, claim.RequestID, err)

				continue
			}
			d.log.InfoS(ctx, "Asset deposit claimed",
				slog.String("request_id", claim.RequestID),
				slog.String(
					"claim_txid", claim.ClaimTxid.String(),
				),
			)

			continue
		}

		d.claimsMu.Lock()
		_, tracked := d.trackedClaims[claim.ClaimTxid]
		d.claimsMu.Unlock()
		if tracked {
			continue
		}

		if err := d.retrackClaim(ctx, claim); err != nil {
			d.log.DebugS(ctx, "Unable to resume asset deposit "+
				"claim",
				slog.String("request_id", claim.RequestID),
				btclog.Fmt("error", "%v", err),
			)
		}
	}
}

// retrackClaim rebuilds a recorded claim from its journal and hands it to
// txconfirm.
func (d *assetDepositService) retrackClaim(ctx context.Context,
	claim *db.AssetDepositClaim) error {

	deposit, err := d.store.GetAssetDeposit(ctx, claim.RequestID)
	if err != nil {
		return err
	}
	result, err := d.resumeClaim(ctx, deposit)
	if err != nil {
		return err
	}
	if result.Tx.TxHash() != claim.ClaimTxid {
		return fmt.Errorf("journaled claim %s does not match recorded "+
			"claim %s", result.Tx.TxHash(), claim.ClaimTxid)
	}

	return d.trackClaim(ctx, deposit, result.Tx)
}

// claimFinal reports whether tapd recorded the claim transaction in a block
// at least claimFinalityDepth deep below tip.
func (d *assetDepositService) claimFinal(ctx context.Context,
	txid chainhash.Hash, tip uint32) (bool, error) {

	transfers, err := d.tapd.ListTransfers(
		ctx, &tapsdk.ListTransfersRequest{
			AnchorTxid: txid.String(),
		},
	)
	if err != nil {
		return false, err
	}

	var (
		height    uint32
		confirmed bool
	)
	for _, transfer := range transfers {
		if transfer != nil &&
			transfer.TransferTxid == [32]byte(txid) &&
			transfer.AnchorTxBlockHash != [32]byte{} {

			height = transfer.AnchorTxBlockHeight
			confirmed = true

			break
		}
	}

	return confirmed && tip >= height &&
		tip-height+1 >= claimFinalityDepth, nil
}

// depositClaimRequest spends a deposit output through its owner's exit leaf.
// The asset commitment is the exit leaf's top-level sibling, so the spend
// binds to the composed output.
func depositClaimRequest(deposit *db.AssetDeposit) (*tapassets.ClaimRequest,
	error) {

	output := deposit.Output
	exitSpend, err := arkscript.ComposedBoardingExitSpend(
		output.OwnerKey.PubKey, deposit.Request.OperatorKey,
		deposit.Request.ExitDelay, output.TaprootAssetRoot,
	)
	if err != nil {
		return nil, err
	}

	return &tapassets.ClaimRequest{
		RequestID: assetClaimJournalPrefix + deposit.Request.RequestID,
		Outpoint:  output.Outpoint,
		Output: wire.NewTxOut(
			output.ValueSat, output.PkScript,
		),
		ExitSpend:     exitSpend,
		OwnerKey:      output.OwnerKey,
		AssetRef:      tapsdk.AssetRef(deposit.Request.AssetRef),
		Amount:        deposit.Request.AssetAmount,
		ProofFile:     deposit.ProofFile,
		OPTrueWitness: output.OPTrueWitness,
	}, nil
}

// applyAssetDepositClaim adds a deposit's claim to its RPC form.
func applyAssetDepositClaim(resp *waverpc.AssetDeposit,
	claim *db.AssetDepositClaim) *waverpc.AssetDeposit {

	if claim == nil {
		return resp
	}
	resp.ClaimTxid = claim.ClaimTxid.String()
	resp.Claimed = claim.Status == db.AssetDepositClaimed

	return resp
}

// claimSigner signs deposit claims with the daemon wallet, which holds the
// deposit owner keys. The wallet adapter resolves at signing time, after the
// wallet is ready.
type claimSigner struct {
	server *Server
}

// SignOutputRaw signs one claim input with the daemon wallet.
func (c claimSigner) SignOutputRaw(tx *wire.MsgTx,
	signDesc *input.SignDescriptor) (input.Signature, error) {

	signer, err := c.server.newSweepWallet()
	if err != nil {
		return nil, err
	}

	return signer.SignOutputRaw(tx, signDesc)
}

// broadcastAssetClaim hands a zero-fee claim to txconfirm, which pays its fee
// with a CPFP child from the daemon wallet until it confirms.
func (s *Server) broadcastAssetClaim(ctx context.Context, tx *wire.MsgTx,
	heightHint uint32) error {

	if s.actorSystem == nil || s.assetClaimSubscriber == nil {
		return fmt.Errorf("actor system unavailable")
	}

	future := txconfirm.LookupRef(s.actorSystem).Ask(
		ctx, &txconfirm.EnsureConfirmedReq{
			Tx:                   tx,
			ConfirmationPkScript: tx.TxOut[0].PkScript,
			Label:                "asset deposit claim",
			HeightHint:           heightHint,
			TargetConfs:          1,
			RetryUntilAccepted:   true,
			Subscriber:           s.assetClaimSubscriber,
		},
	)
	result := future.Await(ctx)
	if result.IsErr() {
		return fmt.Errorf("ensure confirmed: %w", result.Err())
	}
	resp, ok := result.UnwrapOr(nil).(*txconfirm.EnsureConfirmedResp)
	if !ok || resp == nil {
		return fmt.Errorf("ensure confirmed: unexpected response")
	}
	if resp.State == txconfirm.TxStateFailed {
		return fmt.Errorf("txconfirm failed the claim at registration")
	}

	return nil
}

// reserveAssetDepositClaim reserves a deposit for a claim with the round
// actor.
func (s *Server) reserveAssetDepositClaim(ctx context.Context,
	outpoint wire.OutPoint) error {

	if s.actorSystem == nil {
		return fmt.Errorf("actor system not initialized")
	}

	roundRef := round.NewServiceKey().Ref(s.actorSystem)

	return roundRef.Ask(
		ctx, &round.ReserveAssetDepositClaimRequest{
			Outpoint: outpoint,
		},
	).Await(ctx).Err()
}

// registerAssetClaimSubscriber registers the actor that wakes the deposit
// watcher whenever txconfirm reports on a claim.
func (s *Server) registerAssetClaimSubscriber(
	deposits *assetDepositService) claimNotifications {

	behavior := actor.NewFunctionBehavior(
		func(context.Context,
			txconfirm.Notification) fn.Result[struct{}] {

			deposits.requestPoll()

			return fn.Ok(struct{}{})
		},
	)

	return actor.RegisterWithSystem(
		s.actorSystem, assetClaimSubscriberName,
		actor.NewServiceKey[txconfirm.Notification, struct{}](
			assetClaimSubscriberName,
		),
		behavior,
	)
}
