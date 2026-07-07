package txconfirm

import (
	"context"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightninglabs/darepo-client/chainsource"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

// maybeEnsureFeeInputSupply reacts to a failed CPFP child broadcast by
// kicking off (or refreshing) a fee-input fanout. It is the single entry point
// from the broadcast path into the fanout subsystem: the actor calls it
// whenever a tracked tx's broadcast attempt returns an error, and it filters
// for the one error that a fanout can actually resolve.
//
// All steps are best-effort and log-on-failure rather than propagating: the
// triggering broadcast already failed and will be retried on the next
// interval, so a transient inability to fan out must not escalate. The actor's
// "never give up on a no-mempool tx" invariant relies on this staying quiet.
func (a *TxBroadcasterActor) maybeEnsureFeeInputSupply(ctx context.Context,
	err error) {

	// Only a confirmed fee-input shortfall is fixable by a fanout. Any
	// other broadcast error (a structurally bad parent, a transient relay
	// reject) is left to the normal retry path; fanning out would not help.
	if !errors.Is(err, ErrCPFPFeeInputUnavailable) {
		return
	}
	if !errors.Is(err, errCPFPFeeInputShortfall) {
		return
	}

	// Translate the actor's tracked anchor parents into per-parent fee
	// demands. This reads actor state (a.tracked) and estimates fees, so it
	// lives here rather than in the pure-IO FSM.
	demands, feeRate, err := a.activeFeeInputDemands(ctx)
	if err != nil {
		a.log.WarnS(ctx, "Unable to compute fee-input demand",
			err)

		return
	}
	if len(demands) == 0 {
		return
	}

	// Hand the demand set to the fanout FSM, which owns the supply decision
	// and the at-most-one-in-flight fanout. The FSM does its own wallet and
	// broadcast IO inside the transition and returns the conf-watch
	// register/unregister effects as outbox events that driveFeeBump
	// applies on the actor's behalf.
	a.driveFeeBump(ctx, &feeBumpDemandsObserved{
		demands:       demands,
		feeRate:       feeRate,
		height:        a.bestHeight,
		retryInterval: a.cfg.FeeBumpIntervalBlocks,
	})
}

// driveFeeBump feeds one event into the fanout FSM and processes the outbox it
// hands back. Because the txconfirm actor serializes every fanout event, the
// FSM transition (which does its own blocking wallet/broadcast IO) runs to
// completion synchronously here, and the accumulated outbox events come back
// via the Ask future. The FSM only emits outbox events for effects that touch
// actor-owned resources — the chainsource confirmation watch and the tracked-tx
// retry loop — so those are the only cases handled below.
func (a *TxBroadcasterActor) driveFeeBump(ctx context.Context,
	event feeBumpEvent) {

	if a.feeBumpFSM == nil {
		return
	}

	a.ensureFeeBumpStarted(ctx)

	outbox, err := a.feeBumpFSM.AskEvent(ctx, event).Await(ctx).Unpack()
	if err != nil {
		a.log.WarnS(ctx, "Fee-input fanout event failed", err)

		return
	}

	// A transition that hit an operational failure (a rejected broadcast, a
	// rewritten output) stashes the error on the environment rather than
	// returning it, so the long-lived FSM survives. Surface it here so the
	// failure is not silently swallowed.
	if turnErr := a.feeBumpEnv.takeLastErr(); turnErr != nil {
		a.log.WarnS(ctx, "Unable to fan out fee inputs", turnErr)
	}

	for _, out := range outbox {
		switch out := out.(type) {
		// A fresh fanout is on the wire: arm a confirmation watch so
		// the actor learns when its outputs mature into usable fee
		// inputs.
		case *feeBumpWatchFanout:
			if err := a.registerFanoutConfWatch(
				ctx, out.txid, out.watchScript,
			); err != nil {

				a.log.WarnS(ctx, "Unable to watch fee-input "+
					"fanout", err, "txid", out.txid)
			}

		// The fanout confirmed or was abandoned: tear down its watch.
		case *feeBumpUnwatchFanout:
			if err := a.unregisterFanoutConfWatch(
				ctx, out.txid, out.watchScript,
			); err != nil {

				a.log.WarnS(ctx, "Failed to unregister fanout "+
					"watch", err, "txid", out.txid)
			}

		// Fresh fee inputs are available: retry every parent that was
		// stuck waiting on supply.
		case *feeBumpRetryParents:
			a.retryBroadcastingParents(ctx)
		}
	}
}

// ensureFeeBumpStarted lazily starts the fanout FSM the first time it is
// driven. The supplied context is the actor's long-lived internal context (the
// same one used to start the per-tracked-tx FSMs), so the fanout FSM goroutine
// outlives any individual request that happens to trigger the first fanout.
func (a *TxBroadcasterActor) ensureFeeBumpStarted(ctx context.Context) {
	if a.feeBumpFSM == nil || a.feeBumpFSM.IsRunning() {
		return
	}

	a.feeBumpFSM.Start(ctx)
}

// feeBumpPending returns the in-flight fanout the FSM is currently tracking, or
// nil if it is idle (or not running). It is the actor's read accessor for the
// FSM-owned pending state, used to check confirmation ownership.
func (a *TxBroadcasterActor) feeBumpPending() *pendingFeeInputFanout {
	if a.feeBumpFSM == nil || !a.feeBumpFSM.IsRunning() {
		return nil
	}

	rawState, err := a.feeBumpFSM.CurrentState()
	if err != nil {
		return nil
	}

	state, ok := rawState.(feeBumpState)
	if !ok {
		return nil
	}

	return feeBumpPendingFanout(state)
}

// activeFeeInputDemands walks the actor's tracked transactions and builds the
// set of anchor parents that currently need a confirmed wallet fee input,
// along with the shared fee rate the fanout should size against. Only
// non-terminal, anchor-bearing parents qualify: a confirmed or failed tx needs
// nothing, and a tx without an anchor output is broadcast directly with no
// CPFP child and so never consumes a fee input.
//
// Each demand's minAmount is the parent's CPFP package fee plus the dust limit,
// so a single fanout output is guaranteed to both cover the child fee and leave
// a spendable (non-dust) change output.
func (a *TxBroadcasterActor) activeFeeInputDemands(ctx context.Context) (
	[]feeInputDemand, int64, error) {

	feeRate, err := a.broadcaster.EstimateFeeRate(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("estimate fee: %w", err)
	}

	var demands []feeInputDemand
	for txid, entry := range a.tracked {
		state, err := entry.currentTxState()
		if err != nil {
			continue
		}

		// Terminal parents have no future child to fund.
		if state == TxStateConfirmed || state == TxStateFailed {
			continue
		}

		// Only anchor (CPFP) parents pull a fee input; direct-broadcast
		// parents pay their own way.
		if findAnchorOutput(entry.data.Tx) < 0 {
			continue
		}

		fee, err := EstimatePackageFee(
			entry.data.Tx, btcutil.Amount(feeRate),
		)
		if err != nil {
			return nil, 0, err
		}

		demands = append(demands, feeInputDemand{
			parentTxid: txid,
			minAmount:  fee + DustLimit,
		})
	}

	return demands, feeRate, nil
}

// registerFanoutConfWatch arms a chainsource confirmation watch on the
// fanout's chosen output script. When the fanout confirms, chainsource Tells
// the actor a confirmationObservedMsg (via the mapped notify ref), which routes
// into handleFanoutConfirmed. The watch is keyed by both txid and pkScript so
// it round-trips symmetrically with unregisterFanoutConfWatch.
func (a *TxBroadcasterActor) registerFanoutConfWatch(ctx context.Context,
	txid chainhash.Hash, watchScript []byte) error {

	if len(watchScript) == 0 {
		return nil
	}

	notifyRef := chainsource.MapConfirmationEvent(
		a.selfRef,
		func(event chainsource.ConfirmationEvent) Msg {
			return &confirmationObservedMsg{
				txid:        event.Txid,
				blockHeight: event.BlockHeight,
				numConfs:    event.NumConfs,
			}
		},
	)

	watchScript = append([]byte(nil), watchScript...)
	_, err := a.cfg.ChainSource.Ask(
		ctx, &chainsource.RegisterConfRequest{
			CallerID:    a.fanoutConfCallerID(txid),
			Txid:        &txid,
			PkScript:    watchScript,
			TargetConfs: 1,
			NotifyActor: fn.Some(notifyRef),
		},
	).Await(ctx).Unpack()

	return err
}

// unregisterFanoutConfWatch tears down the confirmation watch armed by
// registerFanoutConfWatch. It passes the same txid + pkScript so chainsource's
// txid+script keyed service-actor lookup resolves the exact watch to cancel,
// avoiding a leaked sub-actor once the fanout has confirmed or been abandoned.
func (a *TxBroadcasterActor) unregisterFanoutConfWatch(ctx context.Context,
	txid chainhash.Hash, watchScript []byte) error {

	if len(watchScript) == 0 {
		return nil
	}

	watchScript = append([]byte(nil), watchScript...)
	_, err := a.cfg.ChainSource.Ask(
		ctx, &chainsource.UnregisterConfRequest{
			CallerID:    a.fanoutConfCallerID(txid),
			Txid:        &txid,
			PkScript:    watchScript,
			TargetConfs: 1,
		},
	).Await(ctx).Unpack()

	return err
}

// handleFanoutConfirmed processes a confirmation callback, returning true when
// the event was a fanout confirmation this actor owns (so the generic
// confirmation handler can stop routing it onward). A confirmation for some
// other txid, or one arriving after the fanout was already cleared, is not
// ours: return false and let the normal tracked-tx path handle it.
//
// On a match it drives the fanout FSM with the confirmation event. The FSM
// promotes the now-confirmed predicted outputs into usable fee inputs and
// returns the unwatch + retry-parents effects as outbox events, which
// driveFeeBump applies (tearing down the watch and retrying the stuck parents).
func (a *TxBroadcasterActor) handleFanoutConfirmed(ctx context.Context,
	msg *confirmationObservedMsg) bool {

	pending := a.feeBumpPending()
	if pending == nil || pending.txid != msg.txid {
		return false
	}

	a.driveFeeBump(ctx, &feeBumpFanoutConfirmedEvent{
		txid: msg.txid,
	})

	return true
}

// retryBroadcastingParents re-attempts every tracked tx still stuck in the
// Broadcasting state now that the fanout has confirmed and fresh fee inputs are
// available. A parent that previously failed with ErrCPFPFeeInputUnavailable
// stayed in Broadcasting (the "never give up on a no-mempool tx" invariant), so
// this is the point where it can finally build its CPFP child and land.
func (a *TxBroadcasterActor) retryBroadcastingParents(ctx context.Context) {
	for _, entry := range a.tracked {
		state, err := entry.currentTxState()
		if err != nil || state != TxStateBroadcasting {
			continue
		}

		_, err = a.broadcastTrackedTx(ctx, entry, TxStateBroadcasting)
		a.recordInitialBroadcastOutcome(ctx, entry, err)
	}
}

// fanoutConfCallerID derives the deterministic chainsource caller id for a
// fanout's confirmation watch. It is stable for a given fanout txid so the
// register and unregister calls address the same watch.
func (a *TxBroadcasterActor) fanoutConfCallerID(txid chainhash.Hash) string {
	return "txconfirm-fee-input-fanout-" + txid.String()
}
