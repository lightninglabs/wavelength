package waved

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/arkchannel"
	"github.com/lightninglabs/wavelength/lnruntime"
	"github.com/lightningnetwork/lnd/chanstate"
	"github.com/lightningnetwork/lnd/contractcourt"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var (
	// ErrInsufficientArkChannelLiquidity means no active private channel
	// has enough balance on the requested sending side.
	ErrInsufficientArkChannelLiquidity = fmt.Errorf("insufficient active " +
		"Ark channel liquidity")

	// ErrReceiveChannelFallback means channel creation was abandoned before
	// the hub's prepared OOR crossed its signing point of no return.
	ErrReceiveChannelFallback = fmt.Errorf("receive channel can safely " +
		"fall back to vHTLC")
)

const (
	arkChannelReceiveCleanupTimeout = 30 * time.Second
)

// NativeArkChannelController owns one Ark FSM and one modular lnd endpoint.
type NativeArkChannelController struct {
	party       arkchannel.Party
	cfg         ArkChannelControllerConfig
	coordinator *arkchannel.Coordinator
	remote      lnruntime.ProcessFundingPeer
	peerInfo    lnruntime.FundingPeerInfo
	keys        nativeArkChannelKeys
	service     *arkchannel.Service
}

var _ contractcourt.AuxChannelLifecycle = (*NativeArkChannelController)(nil)

// nativeArkChannelKeys are fixed wallet roles restored by locator on restart.
type nativeArkChannelKeys struct {
	ark     keychain.KeyDescriptor
	backing keychain.KeyDescriptor
	funder  keychain.KeyDescriptor
}

// arkChannelBackingRestorer registers virtual funding before lnd restores its
// pending funding manager state.
type arkChannelBackingRestorer interface {
	RestoreBacking(arkchannel.Terms, arkchannel.Backing) error
}

// restoreNativeArkChannelBackingRecords registers every signed backing in one
// pre-start snapshot of the durable channel store.
func restoreNativeArkChannelBackingRecords(restorer arkChannelBackingRestorer,
	records []arkchannel.Record) error {

	failures := make([]arkchannel.ResumeFailure, 0)
	for _, record := range records {
		backing := record.Snapshot.Backing
		if backing == nil ||
			record.Snapshot.Phase == arkchannel.PhaseClosed {

			continue
		}
		if err := restorer.RestoreBacking(
			record.Snapshot.Terms, *backing,
		); err != nil {

			failures = append(failures, arkchannel.ResumeFailure{
				ChannelID: record.Snapshot.Terms.ID,
				Err: fmt.Errorf(
					"restore backing: %w", err,
				),
			})
		}
	}
	if len(failures) > 0 {
		return &arkchannel.ResumeFailures{Failures: failures}
	}

	return nil
}

// arkChannelForceCloseResumer reconciles one materialized channel with lnd's
// durable commitment-broadcast state.
type arkChannelForceCloseResumer interface {
	ResumeForceCloseChannel(wire.OutPoint) error
}

// resumeOnchainArkChannelRecords attempts every materialized channel and
// returns only isolated failures after the complete recovery pass.
func resumeOnchainArkChannelRecords(node arkChannelForceCloseResumer,
	records []arkchannel.Record) error {

	failures := make([]arkchannel.ResumeFailure, 0)
	for _, record := range records {
		if !shouldResumeOnchainArkChannel(record.Snapshot) {
			continue
		}
		if record.Snapshot.Backing == nil {
			failures = append(failures, arkchannel.ResumeFailure{
				ChannelID: record.Snapshot.Terms.ID,
				Err: fmt.Errorf(
					"materialized channel has no " +
						"backing",
				),
			})

			continue
		}
		if err := node.ResumeForceCloseChannel(
			record.Snapshot.Backing.ChannelPoint,
		); err != nil {

			failures = append(failures, arkchannel.ResumeFailure{
				ChannelID: record.Snapshot.Terms.ID,
				Err: fmt.Errorf("resume materialized channel: "+
					"%w",
					err),
			})
		}
	}
	if len(failures) > 0 {
		return &arkchannel.ResumeFailures{Failures: failures}
	}

	return nil
}

// shouldResumeOnchainArkChannel reports whether backing publication crossed
// the durable handoff but lnd may still need its commitment broadcast marker.
func shouldResumeOnchainArkChannel(snapshot arkchannel.Snapshot) bool {
	return snapshot.Phase == arkchannel.PhaseOnChain
}

// ChainWatchOwner reports whether lnd or the Ark lifecycle currently owns
// chain observation for a channel.
func (c *NativeArkChannelController) ChainWatchOwner(ctx context.Context,
	channel *chanstate.OpenChannel) (contractcourt.ChainWatchOwner, error) {

	record, err := c.coordinator.FindByChannelPoint(
		ctx, channel.FundingOutpoint,
	)
	if err != nil {
		return 0, fmt.Errorf("load Ark channel lifecycle: %w", err)
	}

	if shouldWatchArkChannel(record.Snapshot.Phase) {
		return contractcourt.ChainWatchOwnerLnd, nil
	}

	return contractcourt.ChainWatchOwnerAux, nil
}

// PrepareCommitmentPublish materializes the Ark backing before lnd publishes
// a commitment transaction.
func (c *NativeArkChannelController) PrepareCommitmentPublish(
	ctx context.Context, channelPoint wire.OutPoint) error {

	return c.materializeBeforeCommitment(ctx, channelPoint)
}

const arkChannelFinalizationRetry = time.Second

// WaitForChannelFinalization durably records lnd's terminal channel state.
// The Ark owner retains retry policy so lnd only provides the cancellation and
// ordering boundary.
func (c *NativeArkChannelController) WaitForChannelFinalization(
	ctx context.Context, channelPoint wire.OutPoint) error {

	for {
		err := c.recordFullyResolvedChannel(ctx, channelPoint)
		if err == nil {
			return nil
		}

		if c.cfg.Log != nil {
			c.cfg.Log.WarnS(ctx, "Unable to finalize Ark channel",
				err,
				btclog.Fmt("channel_point", "%v", channelPoint),
			)
		}

		timer := time.NewTimer(arkChannelFinalizationRetry)
		select {
		case <-timer.C:
			continue

		case <-ctx.Done():
			timer.Stop()

			return ctx.Err()
		}
	}
}

// materializeBeforeCommitment blocks lnd's local commitment publication until
// either recovery-ready endpoint has durably published the exact backing.
func (c *NativeArkChannelController) materializeBeforeCommitment(
	ctx context.Context, channelPoint wire.OutPoint) error {

	record, err := c.coordinator.FindByChannelPoint(ctx, channelPoint)
	if err != nil {
		return fmt.Errorf("load channel publication lifecycle: %w", err)
	}
	if record.Snapshot.Phase == arkchannel.PhaseOnChain {
		return nil
	}
	if c.service == nil {
		return fmt.Errorf("Ark channel service is not ready")
	}
	record, err = c.service.Materialize(
		ctx, record.Snapshot.Terms.ID,
	)
	if err != nil {
		return fmt.Errorf("materialize channel backing: %w", err)
	}
	if record.Snapshot.Phase != arkchannel.PhaseOnChain {
		return fmt.Errorf("channel backing stopped in phase %s",
			record.Snapshot.Phase)
	}

	return nil
}

// shouldWatchArkChannel reports whether lnd must own the channel's chain and
// contract lifecycle. An unpublished outpoint remains dormant, while an
// observed publication can be handled without first contacting the peer.
func shouldWatchArkChannel(phase arkchannel.Phase) bool {
	switch phase {
	case arkchannel.PhaseActivating, arkchannel.PhaseActive,
		arkchannel.PhaseMaterializing, arkchannel.PhaseOnChain,
		arkchannel.PhaseCoopClosing,
		arkchannel.PhaseCoopCloseSigned,
		arkchannel.PhaseCoopClosePublished:
		return true

	default:
		return false
	}
}

// shouldRestoreArkChannelAddsDisabled reports whether a durable cooperative
// close artifact requires a restored lnd link to remain quiesced.
func shouldRestoreArkChannelAddsDisabled(phase arkchannel.Phase) bool {
	switch phase {
	case arkchannel.PhaseCoopClosing,
		arkchannel.PhaseCoopCloseSigned,
		arkchannel.PhaseCoopClosePublished:
		return true

	default:
		return false
	}
}

// recordFullyResolvedChannel advances the Ark FSM only after lnd has resolved
// and swept every output. Chain evidence can recover a peer that never
// received the initiating endpoint's materialization transition.
func (c *NativeArkChannelController) recordFullyResolvedChannel(
	ctx context.Context, channelPoint wire.OutPoint) error {

	record, err := c.coordinator.FindByChannelPoint(ctx, channelPoint)
	if err != nil {
		return err
	}
	switch record.Snapshot.Phase {
	case arkchannel.PhaseActivating, arkchannel.PhaseActive,
		arkchannel.PhaseMaterializing,
		arkchannel.PhaseCoopClosing,
		arkchannel.PhaseCoopCloseSigned:

		record, _, err = c.coordinator.Apply(
			ctx, record.Snapshot.Terms.ID,
			&arkchannel.BackingObserved{
				TxID: channelPoint.Hash,
			},
		)
		if err != nil {
			return err
		}

	case arkchannel.PhaseOnChain:
	case arkchannel.PhaseClosed:
		return nil

	default:
		return fmt.Errorf("cannot resolve on-chain channel from %s",
			record.Snapshot.Phase)
	}

	_, _, err = c.coordinator.Apply(
		ctx, record.Snapshot.Terms.ID, &arkchannel.ChannelClosed{},
	)

	return err
}

// newPromotionTerms creates stable protocol identifiers and binds every key
// role to the two endpoint wallets.
func (c *NativeArkChannelController) newPromotionTerms(amount btcutil.Amount,
	idempotencyKey string) (arkchannel.Terms, error) {

	if idempotencyKey == "" {
		return arkchannel.Terms{}, fmt.Errorf("idempotency key is " +
			"required")
	}
	id, pending, scid := c.promotionIdentifiers(idempotencyKey)

	return c.newClientFundedTerms(
		id, pending, scid, amount, arkchannel.KindPromotion,
		lntypes.Hash{},
	)
}

// promotionIdentifiers derives every retry-sensitive protocol identifier from
// the daemon identity and caller-provided idempotency key.
func (c *NativeArkChannelController) promotionIdentifiers(
	idempotencyKey string) (arkchannel.ID, [32]byte, uint64) {

	derive := func(label string) [32]byte {
		hash := sha256.New()
		_, _ = hash.Write(
			[]byte("wavelength/ark-channel/promotion/v1/"),
		)
		_, _ = hash.Write([]byte(label))
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write(
			c.cfg.IdentityKey.PubKey.SerializeCompressed(),
		)
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write([]byte(idempotencyKey))
		var result [32]byte
		copy(result[:], hash.Sum(nil))

		return result
	}
	id := arkchannel.ID(derive("channel-id"))
	pending := derive("pending-channel-id")
	scidSeed := derive("reserved-scid")
	blockHeight := uint32(scidSeed[0])<<16 |
		uint32(scidSeed[1])<<8 | uint32(scidSeed[2])
	txIndex := uint32(scidSeed[3])<<16 |
		uint32(scidSeed[4])<<8 | uint32(scidSeed[5])
	if blockHeight == 0 {
		blockHeight = 1
	}
	if txIndex == 0 {
		txIndex = 1
	}
	scid := lnwire.ShortChannelID{
		BlockHeight: blockHeight, TxIndex: txIndex,
		TxPosition: 0,
	}.ToUint64()

	return id, pending, scid
}

// newClientFundedTerms binds deterministic or random protocol identifiers to
// this endpoint's fixed channel and Ark keys.
func (c *NativeArkChannelController) newClientFundedTerms(id arkchannel.ID,
	pending [32]byte, scid uint64, amount btcutil.Amount,
	kind arkchannel.Kind, paymentHash lntypes.Hash) (arkchannel.Terms,
	error) {

	terms := arkchannel.Terms{
		ID: id, Kind: kind, Funder: arkchannel.PartyClient,
		PendingChannelID: pending, ReservedSCID: scid,
		Capacity: amount, PaymentHash: paymentHash,
		VTXO: arkchannel.VTXOTerms{
			ChannelDelay: c.peerInfo.ChannelDelay,
			FunderDelay:  c.peerInfo.FunderDelay,
			MinExitDelay: c.peerInfo.MinimumExitDelay,
		},
	}
	copy(
		terms.ClientNodeKey[:],
		c.cfg.IdentityKey.PubKey.SerializeCompressed(),
	)
	terms.HubNodeKey = c.peerInfo.HubNodeKey
	copy(
		terms.VTXO.ClientArkKey[:],
		c.keys.ark.PubKey.SerializeCompressed(),
	)
	terms.VTXO.HubArkKey = c.peerInfo.HubArkKey
	terms.VTXO.ArkOperatorKey = c.peerInfo.ArkOperatorKey
	copy(
		terms.VTXO.ClientChannelKey[:],
		c.keys.backing.PubKey.SerializeCompressed(),
	)
	terms.VTXO.HubChannelKey = c.peerInfo.HubChannelKey
	copy(
		terms.VTXO.FunderKey[:],
		c.keys.funder.PubKey.SerializeCompressed(),
	)

	return terms, terms.Validate()
}

// newReceiveIntentTerms binds one deterministic invoice reservation to the
// hub's Ark funding key and the client's channel endpoint keys.
func (c *NativeArkChannelController) newReceiveIntentTerms(
	paymentHash lntypes.Hash, reservedSCID uint64,
	capacity btcutil.Amount) (arkchannel.Terms, error) {

	pendingChannelID := arkchannel.ReceiveIntentPendingID(paymentHash)
	terms := arkchannel.Terms{
		ID:               arkchannel.ReceiveIntentID(paymentHash),
		Kind:             arkchannel.KindReceiveIntent,
		Funder:           arkchannel.PartyHub,
		PendingChannelID: pendingChannelID,
		ReservedSCID:     reservedSCID, Capacity: capacity,
		PaymentHash: paymentHash,
		VTXO: arkchannel.VTXOTerms{
			ChannelDelay:   c.peerInfo.ChannelDelay,
			FunderDelay:    c.peerInfo.FunderDelay,
			MinExitDelay:   c.peerInfo.MinimumExitDelay,
			HubArkKey:      c.peerInfo.HubArkKey,
			HubChannelKey:  c.peerInfo.HubChannelKey,
			ArkOperatorKey: c.peerInfo.ArkOperatorKey,
			FunderKey:      c.peerInfo.HubFunderKey,
		},
	}
	copy(
		terms.ClientNodeKey[:],
		c.cfg.IdentityKey.PubKey.SerializeCompressed(),
	)
	terms.HubNodeKey = c.peerInfo.HubNodeKey
	copy(
		terms.VTXO.ClientArkKey[:],
		c.keys.ark.PubKey.SerializeCompressed(),
	)
	copy(
		terms.VTXO.ClientChannelKey[:],
		c.keys.backing.PubKey.SerializeCompressed(),
	)

	return terms, terms.Validate()
}

// waitIncomingPaymentReady joins the two independent durable readiness
// barriers. Returning after only one would either expose an unusable channel or
// release the preimage before its channel exists.
func waitIncomingPaymentReady(ctx context.Context,
	waitInvoice func(context.Context) (lnwire.ShortChannelID, error),
	syncIntent func(context.Context) error) (lnwire.ShortChannelID, error) {

	waitCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	type invoiceWaitResult struct {
		channelID lnwire.ShortChannelID
		err       error
	}
	invoiceResult := make(chan invoiceWaitResult, 1)
	go func() {
		channelID, err := waitInvoice(waitCtx)
		invoiceResult <- invoiceWaitResult{
			channelID: channelID,
			err:       err,
		}
	}()
	syncResult := make(chan error, 1)
	go func() {
		syncResult <- syncIntent(waitCtx)
	}()

	var incomingSCID lnwire.ShortChannelID
	for invoiceResult != nil || syncResult != nil {
		select {
		case result := <-invoiceResult:
			invoiceResult = nil
			if result.err != nil {
				return lnwire.ShortChannelID{}, fmt.Errorf(
					"wait for incoming hold invoice: %w",
					result.err)
			}
			incomingSCID = result.channelID

		case err := <-syncResult:
			syncResult = nil
			if err != nil && !errors.Is(
				err, ErrReceiveChannelFallback,
			) {
				return lnwire.ShortChannelID{}, fmt.Errorf(
					"synchronize receive channel: %w", err)
			}

		case <-waitCtx.Done():
			return lnwire.ShortChannelID{}, waitCtx.Err()
		}
	}

	if incomingSCID == (lnwire.ShortChannelID{}) {
		return lnwire.ShortChannelID{}, fmt.Errorf("incoming payment " +
			"channel SCID is empty")
	}

	return incomingSCID, nil
}

// cancelReceiveIntent applies the same pre-PONR failure to every existing
// endpoint. Intermediate post-PONR states must finish activation before the
// caller can safely commit another winning rail.
func (c *NativeArkChannelController) cancelReceiveIntent(ctx context.Context,
	hash lntypes.Hash, reason string) error {

	id := arkchannel.ReceiveIntentID(hash)
	remote, remoteErr := c.remote.GetFundingChannel(ctx, id)
	remoteMissing := isArkChannelNotFound(remoteErr)
	if remoteErr != nil && !remoteMissing {
		return fmt.Errorf("load remote receive intent: %w", remoteErr)
	}
	local, localErr := c.service.GetChannel(ctx, id)
	localMissing := isArkChannelNotFound(localErr)
	if localErr != nil && !localMissing {
		return fmt.Errorf("load local receive intent: %w", localErr)
	}
	if remoteMissing && localMissing {
		return nil
	}
	if !localMissing && (local.Snapshot.Terms.Kind !=
		arkchannel.KindReceiveIntent ||
		local.Snapshot.Terms.PaymentHash != hash) {
		return fmt.Errorf("channel is not this payment's receive " +
			"intent")
	}
	if (!remoteMissing && !receiveIntentPhaseKnown(remote.Phase)) ||
		(!localMissing &&
			!receiveIntentPhaseKnown(local.Snapshot.Phase)) {
		return fmt.Errorf("receive channel has an unknown phase")
	}

	if (!remoteMissing && receiveIntentIsCommitting(remote.Phase)) ||
		(!localMissing &&
			receiveIntentIsCommitting(local.Snapshot.Phase)) {
		return fmt.Errorf("receive channel is crossing its funding " +
			"safety boundary")
	}
	if receiveIntentEndpointsDisagree(
		remoteMissing, remote.Phase, localMissing, local.Snapshot.Phase,
	) {
		return fmt.Errorf("receive channel endpoints have not " +
			"converged")
	}

	event := &arkchannel.Fail{Reason: reason}
	if !remoteMissing && receiveIntentCanFail(remote.Phase) {
		if _, err := c.remote.ApplyChannelEvent(
			ctx, id, event,
		); err != nil {
			return fmt.Errorf("cancel remote receive intent: %w",
				err)
		}
	}
	if !localMissing && receiveIntentCanFail(local.Snapshot.Phase) {
		if _, err := c.service.ApplyLocalEvent(
			ctx, id, event,
		); err != nil {
			return fmt.Errorf("cancel local receive intent: %w",
				err)
		}
	}

	return nil
}

// isArkChannelNotFound recognizes local and transport-preserved missing rows.
func isArkChannelNotFound(err error) bool {
	return errors.Is(err, arkchannel.ErrNotFound) ||
		status.Code(err) == codes.NotFound
}

// receiveIntentCanFail identifies states where replaying Fail is safe.
func receiveIntentCanFail(phase arkchannel.Phase) bool {
	return phase == arkchannel.PhaseRequested ||
		phase == arkchannel.PhaseNegotiating ||
		phase == arkchannel.PhaseCancelling
}

// receiveIntentIsCommitting identifies the short post-PONR activation window.
func receiveIntentIsCommitting(phase arkchannel.Phase) bool {
	return phase == arkchannel.PhaseBackingReady ||
		phase == arkchannel.PhaseActivating
}

// receiveIntentIsRetained identifies reusable or later channel lifecycle
// states.
func receiveIntentIsRetained(phase arkchannel.Phase) bool {
	switch phase {
	case arkchannel.PhaseActive, arkchannel.PhaseMaterializing,
		arkchannel.PhaseOnChain, arkchannel.PhaseClosed,
		arkchannel.PhaseCoopClosing, arkchannel.PhaseCoopCloseSigned,
		arkchannel.PhaseCoopClosePublished:
		return true

	default:
		return false
	}
}

// receiveIntentPhaseKnown rejects corrupt or future phase values
// conservatively.
func receiveIntentPhaseKnown(phase arkchannel.Phase) bool {
	return receiveIntentCanFail(phase) ||
		receiveIntentIsCommitting(phase) ||
		receiveIntentIsRetained(phase) ||
		phase == arkchannel.PhaseFailed
}

// receiveIntentEndpointsDisagree detects a retained/pre-PONR split.
func receiveIntentEndpointsDisagree(remoteMissing bool, remote arkchannel.Phase,
	localMissing bool, local arkchannel.Phase) bool {

	remoteRetained := !remoteMissing && receiveIntentIsRetained(remote)
	localRetained := !localMissing && receiveIntentIsRetained(local)

	return remoteRetained != localRetained
}

// receiveIntentFundingOutcome is the funder's authoritative durable result.
// Endpoint phases may lag because their peer notification is replayable.
type receiveIntentFundingOutcome uint8

const (
	receiveIntentFundingPending receiveIntentFundingOutcome = iota
	receiveIntentFundingAborted
	receiveIntentFundingFinalized
)

// receiveIntentFunderOutcome reads OOR terminal evidence only from the
// endpoint named as the immutable channel funder.
func receiveIntentFunderOutcome(localParty arkchannel.Party,
	local arkchannel.Snapshot,
	remote lnruntime.FundingChannelState) (receiveIntentFundingOutcome,
	error) {

	var finalized, aborted bool
	var source *arkchannel.VTXOBinding
	var phase arkchannel.Phase
	if local.Terms.Funder == localParty {
		finalized = local.OORFinalized
		aborted = local.OORAborted
		source = local.Source
		phase = local.Phase
	} else {
		finalized = remote.OORFinalized
		aborted = remote.OORAborted
		source = remote.Source
		phase = remote.Phase
	}
	if finalized && aborted {
		return receiveIntentFundingPending, fmt.Errorf("receive " +
			"intent funder reports both finalized and aborted " +
			"OOR funding")
	}
	if finalized {
		return receiveIntentFundingFinalized, nil
	}
	if aborted || (source == nil && phase == arkchannel.PhaseFailed) {
		return receiveIntentFundingAborted, nil
	}

	return receiveIntentFundingPending, nil
}

// failReceiveIntent abandons the funder's prepared OOR and waits for durable
// native funding cleanup. The hub's funding wire acknowledges peer cleanup
// before recording local cancellation; only the client queries the hub by RPC.
func (c *NativeArkChannelController) failReceiveIntent(ctx context.Context,
	id arkchannel.ID, cause error) error {

	reason := cause.Error()
	ticker := time.NewTicker(arkChannelControllerPollInterval)
	defer ticker.Stop()

	var lastErr error
	for {
		local, localErr := c.service.GetChannel(ctx, id)
		var operationErr error
		if localErr == nil {
			switch local.Snapshot.Phase {
			case arkchannel.PhaseRequested,
				arkchannel.PhaseNegotiating,
				arkchannel.PhaseCancelling,
				arkchannel.PhaseFailed:

				operationErr = c.driveReceiveIntentFailure(
					ctx, id, local, reason,
				)

			case arkchannel.PhaseBackingReady:
				var remote lnruntime.FundingChannelState
				if local.Snapshot.Terms.Funder != c.party {
					remote, operationErr =
						c.remote.GetFundingChannel(
							ctx, id,
						)
					if operationErr != nil {
						break
					}
				}
				outcome, err := receiveIntentFunderOutcome(
					c.party, local.Snapshot, remote,
				)
				if err != nil {
					return errors.Join(cause, err)
				}
				if outcome != receiveIntentFundingAborted ||
					local.Snapshot.Source == nil {
					return errors.Join(
						cause,
						fmt.Errorf("receive intent "+
							"crossed its funding "+
							"safety "+
							"boundary at %s",
							local.Snapshot.Phase),
					)
				}
				abortReason := remote.Failure
				if abortReason == "" {
					abortReason = reason
				}
				source := local.Snapshot.Source
				_, operationErr = c.service.ApplyPeerEvent(
					ctx, id, &arkchannel.OORAborted{
						SessionID: source.OORSessionID,
						Reason:    abortReason,
					},
				)

			default:
				return errors.Join(
					cause, fmt.Errorf("receive intent "+
						"crossed its funding safety "+
						"boundary at %s",
						local.Snapshot.Phase),
				)
			}
		} else {
			operationErr = localErr
		}
		if operationErr != nil {
			lastErr = operationErr
		}

		local, localErr = c.service.GetChannel(ctx, id)
		var remote lnruntime.FundingChannelState
		var remoteErr error
		if c.party == arkchannel.PartyClient {
			remote, remoteErr = c.remote.GetFundingChannel(ctx, id)
		}
		if localErr == nil && remoteErr == nil {
			outcome, err := receiveIntentFunderOutcome(
				c.party, local.Snapshot, remote,
			)
			if err != nil {
				return errors.Join(cause, err)
			}
			peerCleaned := c.party == arkchannel.PartyHub ||
				remote.Phase == arkchannel.PhaseFailed
			if local.Snapshot.Phase == arkchannel.PhaseFailed &&
				peerCleaned &&
				outcome == receiveIntentFundingAborted {
				return fmt.Errorf("%w: %w",
					ErrReceiveChannelFallback, cause)
			}
			remoteCrossedBoundary := receiveIntentIsCommitting(
				remote.Phase,
			) && outcome != receiveIntentFundingAborted
			if receiveIntentIsRetained(remote.Phase) ||
				remoteCrossedBoundary {
				return errors.Join(
					cause, fmt.Errorf("remote receive "+
						"intent crossed its funding "+
						"safety boundary at %s",
						remote.Phase),
				)
			}
		} else {
			lastErr = errors.Join(localErr, remoteErr)
		}

		select {
		case <-ctx.Done():
			return errors.Join(cause, lastErr, ctx.Err())

		case <-ticker.C:
		}
	}
}

// driveReceiveIntentFailure first makes the observing endpoint terminal or
// cancelling, then asks the endpoint that funded the source to abort it.
func (c *NativeArkChannelController) driveReceiveIntentFailure(
	ctx context.Context, id arkchannel.ID, local arkchannel.Record,
	reason string) error {

	if local.Snapshot.Failure != "" {
		reason = local.Snapshot.Failure
	}
	var event arkchannel.Event = &arkchannel.Fail{Reason: reason}
	if local.Snapshot.Source == nil &&
		local.Snapshot.Terms.Kind == arkchannel.KindReceiveIntent &&
		local.Snapshot.Terms.Funder != c.party {

		event = &arkchannel.ReceiveIntentAbortRequested{
			Reason: reason,
		}
	}

	_, err := c.service.ApplyLocalEvent(ctx, id, event)

	return err
}

// failReceiveIntentDetached gives pre-PONR cleanup a bounded process-owned
// lifetime after the intercepted-payment request has ended.
func (c *NativeArkChannelController) failReceiveIntentDetached(
	ctx context.Context, id arkchannel.ID, cause error) error {

	cleanupCtx, cancel := context.WithTimeout(
		context.WithoutCancel(ctx), arkChannelReceiveCleanupTimeout,
	)
	defer cancel()

	return c.failReceiveIntent(cleanupCtx, id, cause)
}
