package unroll

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btcwallet/wallet/txrules"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/chainsource"
	"github.com/lightninglabs/wavelength/txconfirm"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

// repriceSweep builds a relayable candidate for the same target and wallet
// destination. Valid candidates require a higher replacement fee; a legacy
// dust payout instead needs a lower fee. The old candidate remains until its
// replacement and history are staged. A flat estimate, exhausted fee budget,
// unavailable signer, or changed policy leaves the obligation pending.
func (b *behavior) repriceSweep(ctx context.Context) (bool, error) {
	old := b.sweepTx
	if old == nil || len(old.TxIn) != 1 || len(old.TxOut) != 1 ||
		old.TxIn[0].PreviousOutPoint != b.cfg.TargetOutpoint {
		return false, fmt.Errorf("replacement requires a " +
			"single-target sweep")
	}
	policy, err := b.resolveExitSpendPolicy(ctx)
	if err != nil {
		return false, err
	}
	// A replacement must use an actual estimate. An emergency fallback
	// cannot demonstrate that the previously rejected fee has recovered.
	rate, err := estimateSweepFeeRate(
		ctx, b.cfg.ChainSource, b.cfg.MaxSweepFeeRateSatPerVByte, 0,
	)
	if err != nil {
		return false, err
	}
	target, err := b.proof.TargetOutput()
	if err != nil {
		return false, err
	}
	oldFee := target.Value - old.TxOut[0].Value
	dust := sweepHasDustOutput(old)
	// Policies own their fee budgets. Judge the returned candidate's actual
	// fee below rather than assuming the standard policy's size estimate.
	candidate, err := policy.BuildSpendTx(ctx, ExitSpendRequest{
		TargetOutpoint: b.cfg.TargetOutpoint,
		TargetOutput:   target,
		DestinationPkScript: append(
			[]byte(nil), old.TxOut[0].PkScript...,
		),
		FeeRateSatPerVByte: rate,
		CurrentHeight:      b.currentHeight(),
		Signer:             b.cfg.Wallet,
	})
	if err != nil {
		return false, err
	}
	if candidate == nil || len(candidate.TxIn) != 1 ||
		len(candidate.TxOut) != 1 ||
		candidate.Version != old.Version ||
		candidate.LockTime != old.LockTime ||
		candidate.TxIn[0].PreviousOutPoint !=
			old.TxIn[0].PreviousOutPoint ||
		candidate.TxIn[0].Sequence != old.TxIn[0].Sequence ||
		!bytes.Equal(
			candidate.TxOut[0].PkScript, old.TxOut[0].PkScript,
		) {
		return false, fmt.Errorf("replacement changed sweep obligation")
	}
	newFee := target.Value - candidate.TxOut[0].Value
	oldSize := (txconfirm.EstimateWeight(old) + 3) / 4
	newSize := (txconfirm.EstimateWeight(candidate) + 3) / 4
	// Apply the direct-spend equivalents of txconfirm's replacement
	// floors: strictly higher feerate and at least 1 sat/vB of additional
	// relay fee. A stricter node can reject again without losing ownership.
	if sweepHasDustOutput(candidate) {
		return false, nil
	}
	if !dust && (newFee-oldFee < newSize ||
		newFee*oldSize <= oldFee*newSize) {
		return false, nil
	}
	b.replacedSweeps = append(b.replacedSweeps, old.Copy())
	b.sweepTx = candidate

	return true, nil
}

// sweepHasDustOutput identifies the direct one-input, one-output sweeps that
// cannot relay under the default dust policy. Anchored exports and asset claims
// have different fee ownership and are excluded by the caller.
func sweepHasDustOutput(tx *wire.MsgTx) bool {
	return tx != nil && len(tx.TxIn) == 1 && len(tx.TxOut) == 1 &&
		(tx.TxOut[0].Value <= 0 || txrules.IsDustOutput(
			tx.TxOut[0], txrules.DefaultRelayFeePerKb,
		))
}

// deferDustSweep checkpoints local rejection before routing cancellation.
// The signed bytes and source spend watch remain available if a miner accepts
// the nonstandard transaction. Later blocks may repair it at a lower fee; an
// unavailable or still uneconomic quote never consumes the failure budget.
func (b *behavior) deferDustSweep(ctx context.Context,
	ax actor.Exec[unrollTx]) (bool, error) {

	if b.isAssetExit() || !sweepHasDustOutput(b.sweepTx) {
		return false, nil
	}
	state, err := b.currentState()
	if err != nil {
		return false, err
	}
	job := stateJob(state)
	txid := b.sweepTx.TxHash()
	if job.RejectedSweep.IsNone() ||
		job.RejectedSweep.UnsafeFromSome() != txid || job.RetrySame {

		if err := b.driveEvent(ctx, ax, &SweepDustEvent{
			Txid: txid,
		}); err != nil {
			return false, err
		}
		state, err = b.currentState()
		if err != nil {
			return false, err
		}
		job = stateJob(state)
		b.log.InfoS(ctx, "Deferring persisted dust sweep",
			slog.String("txid", txid.String()),
		)
	}

	return job.Height <= job.RepriceAfter, nil
}

// cancelSweepInterest stops this exit's retry loop without losing its claim.
func (b *behavior) cancelSweepInterest(ctx context.Context,
	txid chainhash.Hash) error {

	if b.cancelledSweep.IsSome() &&
		b.cancelledSweep.UnsafeFromSome() == txid {
		return nil
	}

	//actor:allow-await ordered cancel ACK; no subscriber callback
	_, err := b.cfg.TxConfirmRef.Ask(ctx, &txconfirm.CancelInterestReq{
		Txid:         txid,
		SubscriberID: b.notificationRef().ID(),
	}).Await(ctx).Unpack()
	if err != nil {
		return fmt.Errorf("cancel rejected sweep interest: %w", err)
	}
	b.cancelledSweep = fn.Some(txid)

	return nil
}

// stopRejectedSweep relinquishes the rejected candidate's active broadcaster
// interest before submitting a replacement. Wallet removal is best-effort
// housekeeping: it cannot prove network absence and some backends retain a
// separate rebroadcaster. Candidate history and the target spend watch preserve
// the old transaction's evidence even if removal fails or it later confirms.
func (b *behavior) stopRejectedSweep(ctx context.Context,
	txid chainhash.Hash) error {

	if err := b.cancelSweepInterest(ctx, txid); err != nil {
		return err
	}
	_, err := b.cfg.ChainSource.Ask(ctx, &chainsource.RemoveTxRequest{
		Txid: txid,
	}).Await(ctx).Unpack()
	if err != nil {
		b.log.WarnS(ctx, "Unable to remove rejected sweep from wallet",
			err,
			slog.String("txid", txid.String()),
		)
	}

	return nil
}

// sweepCandidate returns a signed candidate owned by this obligation. Keeping
// earlier candidates prevents a delayed spend observation from falsely
// classifying our own replacement race as an external spend.
func (b *behavior) sweepCandidate(txid chainhash.Hash) *wire.MsgTx {
	if b.sweepTx != nil && b.sweepTx.TxHash() == txid {
		return b.sweepTx
	}
	for _, candidate := range b.replacedSweeps {
		if candidate.TxHash() == txid {
			return candidate
		}
	}

	return nil
}

// confirmSweepCandidate selects the actual winner before completing the FSM.
// A crash between selection and confirmation reattaches the winner's existing
// confirmation watch rather than building another transaction.
func (b *behavior) confirmSweepCandidate(ctx context.Context,
	ax actor.Exec[unrollTx], txid chainhash.Hash,
	height int32) fn.Result[Resp] {

	if err := b.ensureLoaded(ctx); err != nil {
		return fn.Err[Resp](err)
	}

	candidate := b.sweepCandidate(txid)
	if candidate == nil {
		return b.handleEvent(ctx, ax, &TxConfirmedEvent{
			Txid: txid, Height: height,
		})
	}
	if b.inTerminalState() {
		return fn.Ok[Resp](&AckResp{})
	}
	b.sweepTx = candidate.Copy()
	if err := b.driveEvent(
		ctx, ax, &SweepBroadcastedEvent{
			Txid: txid,
		},
	); err != nil {
		return fn.Err[Resp](err)
	}

	return b.handleEvent(ctx, ax, &TxConfirmedEvent{
		Txid: txid, Height: height,
	})
}

// copySweepCandidates keeps checkpoint snapshots independent of actor state.
func copySweepCandidates(candidates []*wire.MsgTx) []*wire.MsgTx {
	if len(candidates) == 0 {
		return nil
	}
	result := make([]*wire.MsgTx, len(candidates))
	for i, candidate := range candidates {
		result[i] = candidate.Copy()
	}

	return result
}
