package round

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"

	btcaddr "github.com/btcsuite/btcd/address/v2"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/baselib/protofsm"
	"github.com/lightninglabs/wavelength/lib/actormsg"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/lightninglabs/wavelength/wallet"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
)

// ErrBoardingInFlight rejects a boarding input that a tracked round already
// spends.
var ErrBoardingInFlight = errors.New("boarding input is already in a round")

// ErrDepositClaimed rejects an asset deposit that is reserved for a claim.
var ErrDepositClaimed = errors.New("asset deposit is reserved for a claim")

// ErrRoundBusy rejects an asset deposit while another round of this client is
// joining, because the operator keeps one registration per client and the
// deposit's join would replace it.
var ErrRoundBusy = errors.New("another round is joining")

// AssetBoarding describes a confirmed asset deposit to spend as a round
// boarding input.
type AssetBoarding struct {
	// Outpoint is the deposit output.
	Outpoint wire.OutPoint

	// Value is the deposit output's Bitcoin value.
	Value btcutil.Amount

	// PkScript is the deposit output's script.
	PkScript []byte

	// OwnerKey, OperatorKey and ExitDelay are the boarding policy's
	// parameters.
	OwnerKey    keychain.KeyDescriptor
	OperatorKey *btcec.PublicKey
	ExitDelay   uint32

	// ConfHeight is the deposit's confirmation height.
	ConfHeight int32

	// AssetRef and AssetAmount identify the deposited units.
	AssetRef    string
	AssetAmount uint64

	// AssetDigest scopes the deposit's OP_TRUE asset script key.
	AssetDigest [32]byte

	// AssetProof is the deposit's confirmed proof file.
	AssetProof []byte

	// CommitmentLeafHash is the asset commitment leaf the deposit output
	// branches with its boarding policy.
	CommitmentLeafHash [32]byte

	// AssetWitness is the OP_TRUE witness that spends the deposited asset.
	AssetWitness [][]byte
}

// NewAssetBoardingIntent builds the boarding intent of an asset deposit. It
// fails unless the boarding policy and commitment leaf reproduce the deposit
// script, and it proves the deposit for join-round authorization through the
// composed exit spend.
func NewAssetBoardingIntent(deposit *AssetBoarding,
	params *chaincfg.Params) (BoardingIntent, error) {

	if deposit == nil || deposit.OwnerKey.PubKey == nil ||
		deposit.OperatorKey == nil {
		return BoardingIntent{}, fmt.Errorf("asset boarding is " +
			"incomplete")
	}

	policyTemplate, err := arkscript.EncodeStandardVTXOTemplate(
		deposit.OwnerKey.PubKey, deposit.OperatorKey, deposit.ExitDelay,
	)
	if err != nil {
		return BoardingIntent{}, fmt.Errorf("encode boarding "+
			"policy: %w", err)
	}
	pkScript, _, _, err := arkscript.ComposedBoardingScript(
		policyTemplate, deposit.CommitmentLeafHash,
	)
	if err != nil {
		return BoardingIntent{}, err
	}
	if !bytes.Equal(pkScript, deposit.PkScript) {
		return BoardingIntent{}, fmt.Errorf("boarding policy and "+
			"asset commitment do not reproduce deposit %s",
			deposit.Outpoint)
	}
	address, err := btcaddr.NewAddressTaproot(pkScript[2:], params)
	if err != nil {
		return BoardingIntent{}, fmt.Errorf("deposit address: %w", err)
	}

	authSpend, err := arkscript.ComposedBoardingExitSpend(
		deposit.OwnerKey.PubKey, deposit.OperatorKey, deposit.ExitDelay,
		deposit.CommitmentLeafHash,
	)
	if err != nil {
		return BoardingIntent{}, err
	}

	outpoint := deposit.Outpoint
	request := types.BoardingRequest{
		Outpoint:       &outpoint,
		PolicyTemplate: policyTemplate,
		AssetRef:       deposit.AssetRef,
		AssetAmount:    deposit.AssetAmount,
		AssetDigest:    bytes.Clone(deposit.AssetDigest[:]),
		AssetProof:     bytes.Clone(deposit.AssetProof),
		AssetCommitmentLeafHash: bytes.Clone(
			deposit.CommitmentLeafHash[:],
		),
		AssetWitness: cloneAssetWitness(deposit.AssetWitness),
	}
	if err := request.ValidateAssetFields(); err != nil {
		return BoardingIntent{}, err
	}
	if request.AssetRef == "" {
		return BoardingIntent{}, fmt.Errorf("asset boarding has no " +
			"asset")
	}

	return BoardingIntent{
		BoardingIntent: wallet.BoardingIntent{
			Address: wallet.BoardingAddress{
				Address:     address,
				KeyDesc:     deposit.OwnerKey,
				OperatorKey: deposit.OperatorKey,
				ExitDelay:   deposit.ExitDelay,
			},
			Outpoint: outpoint,
			ChainInfo: wallet.BoardingChainInfo{
				ConfHeight: deposit.ConfHeight,
				OutPoint:   outpoint,
				Amount:     deposit.Value,
			},
			Status: wallet.BoardingStatusConfirmed,
		},
		Request:   request,
		AuthSpend: authSpend,
	}, nil
}

// handleAssetBoarding registers an asset deposit with a round of its own,
// together with the asset VTXO that carries its units and the BTC VTXO that
// takes the rest of its value, and requests the join in the same turn. Both
// VTXOs are booked as transfers in, because the deposit's satoshis come from
// outside the daemon's wallet, so the round takes no other intents: their
// inputs would flow into the change VTXO.
func (a *RoundClientActor) handleAssetBoarding(ctx context.Context,
	msg *RegisterAssetBoardingRequest) fn.Result[actormsg.RoundActorResp] {

	intent := msg.Boarding
	if intent.Request.AssetRef == "" || intent.AuthSpend == nil {
		return fn.Err[actormsg.RoundActorResp](
			fmt.Errorf("asset boarding intent is incomplete"),
		)
	}
	if msg.AssetVTXOAmount <= 0 || msg.ChangeAmount <= 0 {
		return fn.Err[actormsg.RoundActorResp](
			fmt.Errorf("asset boarding amounts must be positive"),
		)
	}
	if _, ok := a.claimedDeposits[intent.Outpoint]; ok {
		return fn.Err[actormsg.RoundActorResp](
			fmt.Errorf("%w: %s", ErrDepositClaimed,
				intent.Outpoint),
		)
	}
	if err := a.checkAssetBoarding(ctx, intent.Outpoint); err != nil {
		return fn.Err[actormsg.RoundActorResp](err)
	}

	// The asset VTXO's script depends on the commitment root disclosed
	// with the tree, so it is registered as owned only once verified.
	assetReq, err := a.deriveVTXORequest(
		ctx, msg.AssetVTXOAmount, types.VTXOOriginRoundTransfer,
	)
	if err != nil {
		return fn.Err[actormsg.RoundActorResp](
			fmt.Errorf("build asset VTXO request: %w", err),
		)
	}
	assetReq.AssetRef = intent.Request.AssetRef
	assetReq.AssetAmount = intent.Request.AssetAmount
	assetReq.FixedAmount = true
	if err := validateAssetRequest(assetReq); err != nil {
		return fn.Err[actormsg.RoundActorResp](
			fmt.Errorf("asset VTXO request: %w", err),
		)
	}

	changeReq, err := a.buildVTXORequest(
		ctx, msg.ChangeAmount, types.VTXOOriginRoundTransfer,
	)
	if err != nil {
		return fn.Err[actormsg.RoundActorResp](
			fmt.Errorf("build BTC VTXO request: %w", err),
		)
	}
	changeReq.IsChange = true

	roundFSM, err := a.createNewRound(ctx)
	if err != nil {
		return fn.Err[actormsg.RoundActorResp](
			fmt.Errorf("create round for asset boarding: %w", err),
		)
	}
	roundFSM.AssetDeposit = true

	a.log.InfoS(ctx, "Registering asset deposit",
		slog.String("outpoint", intent.Outpoint.String()),
		slog.String("asset_ref", intent.Request.AssetRef),
		slog.Uint64("asset_amount", intent.Request.AssetAmount),
	)

	pkg := &IntentPackage{Intents: Intents{
		Boarding: []BoardingIntent{
			intent,
		},
		VTXOs: []types.VTXORequest{
			*assetReq,
			*changeReq,
		},
	}}
	err = a.askEventAndProcessOutbox(ctx, roundFSM, pkg)
	if err == nil {
		err = a.askEventAndProcessOutbox(
			ctx, roundFSM, &IntentRequested{},
		)
	}
	if err != nil {
		// A round left holding the deposit would refuse every retry.
		a.removeRound(roundFSM)

		return fn.Err[actormsg.RoundActorResp](
			fmt.Errorf("join round with asset deposit: %w", err),
		)
	}

	state, err := fsmState(ctx, roundFSM.FSM)
	if err != nil {
		return fn.Err[actormsg.RoundActorResp](
			fmt.Errorf("read asset deposit round state: %w", err),
		)
	}
	if failed, ok := state.(*ClientFailedState); ok {
		err := fmt.Errorf("asset deposit round failed: %s",
			failed.Reason)

		return fn.Err[actormsg.RoundActorResp](
			errors.Join(err, failed.Error),
		)
	}

	return fn.Ok[actormsg.RoundActorResp](&RegisterVTXORequestsResponse{
		Success: true,
	})
}

// handleReserveAssetDepositClaim reserves an asset deposit for a claim unless
// a tracked round spends it. Asset boarding refuses a reserved deposit, so a
// claim and a round never both spend one. The reservation lasts for the life
// of the process; the caller records the claim durably before publishing it.
func (a *RoundClientActor) handleReserveAssetDepositClaim(ctx context.Context,
	m *ReserveAssetDepositClaimRequest) fn.Result[actormsg.RoundActorResp] {

	if _, err := a.scanAssetBoarding(ctx, m.Outpoint); err != nil {
		return fn.Err[actormsg.RoundActorResp](err)
	}
	a.claimedDeposits[m.Outpoint] = struct{}{}

	return fn.Ok[actormsg.RoundActorResp](nil)
}

// checkAssetBoarding fails unless an asset deposit can join a round of its
// own: no tracked round spends the deposit, and no other round has a join in
// flight. A round whose state cannot be read fails the check.
func (a *RoundClientActor) checkAssetBoarding(ctx context.Context,
	outpoint wire.OutPoint) error {

	busy, err := a.scanAssetBoarding(ctx, outpoint)
	if err != nil {
		return err
	}
	if busy {
		return ErrRoundBusy
	}

	return nil
}

// scanAssetBoarding fails with ErrBoardingInFlight if a tracked round spends
// the deposit, and reports whether any round has a join in flight. A round
// whose state cannot be read fails the scan.
func (a *RoundClientActor) scanAssetBoarding(ctx context.Context,
	outpoint wire.OutPoint) (bool, error) {

	scanCtx, cancel := fsmScanContext(ctx)
	defer cancel()

	busy := false
	for _, roundFSM := range a.rounds {
		state, err := fsmState(scanCtx, roundFSM.FSM)
		if err != nil {
			return false, fmt.Errorf("read state of round %s: %w",
				roundFSM.Key.KeyString(), err)
		}
		for _, intent := range stateBoarding(state) {
			if intent.Outpoint == outpoint {
				return false, fmt.Errorf("%w: %s",
					ErrBoardingInFlight, outpoint)
			}
		}
		busy = busy || joinInFlight(state)
	}

	return busy, nil
}

// removeRound stops a round's state machine and drops it from the tracked
// rounds.
func (a *RoundClientActor) removeRound(roundFSM *RoundFSM) {
	keyStr := RoundKeyStr(roundFSM.Key.KeyString())
	if _, ok := a.rounds[keyStr]; !ok {
		return
	}

	roundFSM.FSM.Stop()
	delete(a.rounds, keyStr)
}

// joinInFlight reports whether a round has requested its join and the
// operator has not yet sent its commitment transaction.
func joinInFlight(
	state protofsm.State[
		ClientEvent,
		ClientOutMsg,
		*ClientEnvironment,
	]) bool {

	switch s := state.(type) {
	case *PendingRoundAssembly:
		return !s.BatchWait.Cutoff.IsZero()

	case *IntentSentState, *QuoteReceivedState, *RoundJoinedState:
		return true

	default:
		return false
	}
}

// stateBoarding returns the boarding intents a round state spends.
func stateBoarding(
	state protofsm.State[
		ClientEvent,
		ClientOutMsg,
		*ClientEnvironment,
	]) []BoardingIntent {

	switch s := state.(type) {
	case *PendingRoundAssembly:
		return s.Boarding

	case *IntentSentState:
		return s.Intents.Boarding

	case *QuoteReceivedState:
		return s.Intents.Boarding

	case *RoundJoinedState:
		return s.Intents.Boarding

	case *CommitmentTxReceivedState:
		return s.Intents.Boarding

	case *CommitmentTxValidatedState:
		return s.Intents.Boarding

	case *ForfeitSignaturesCollectingState:
		return s.Intents.Boarding

	case *NoncesSentState:
		return s.Intents.Boarding

	case *NoncesAggregatedState:
		return s.Intents.Boarding

	case *PartialSigsSentState:
		return s.Intents.Boarding

	case *InputSigSentState:
		return s.Intents.Boarding

	default:
		return nil
	}
}
