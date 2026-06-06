package unroll

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightninglabs/darepo-client/chainsource"
	"github.com/lightninglabs/darepo-client/unrollplan"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

// PlanFeeInputs delegates ready-frontier fee-input planning to the
// registry-owned coordinator.
func (b *behavior) PlanFeeInputs(ctx context.Context,
	ready []unrollplan.TxFrontier) (FeeInputPlan, error) {

	if b.cfg.FeeInputFanoutCoordinator == nil {
		return FeeInputPlan{
			RequiredFeeInputsNow: len(ready),
		}, nil
	}

	return b.cfg.FeeInputFanoutCoordinator.PlanFeeInputsForChild(
		ctx, b.cfg.ActorID, ready,
	)
}

// startFeeInputFanout creates or waits for a backing-wallet fanout transaction
// and registers this child actor for its confirmation.
func (b *behavior) startFeeInputFanout(ctx context.Context,
	evt *RequestFeeInputFanout) error {

	if evt == nil {
		return fmt.Errorf("fee-input fanout request missing")
	}

	if b.cfg.FeeInputFanoutCoordinator == nil {
		return fmt.Errorf("fee-input fanout coordinator missing")
	}

	var (
		txid     chainhash.Hash
		pkScript []byte
		err      error
	)
	if pending := evt.Plan.PendingFanoutTxid; pending.IsSome() {
		txid = pending.UnsafeFromSome()
		pkScript = append(
			[]byte(nil), evt.Plan.PendingFanoutPkScript...,
		)
	} else {
		txid, pkScript, err =
			b.cfg.FeeInputFanoutCoordinator.EnsureFanout(
				ctx, evt.Plan,
			)
		if err != nil {
			return err
		}
	}

	if len(pkScript) == 0 {
		return fmt.Errorf("fee-input fanout watch pkscript missing")
	}

	notifyRef := chainsource.MapConfirmationEvent(
		b.selfRef, func(event chainsource.ConfirmationEvent) Msg {
			return &FeeInputsAvailableMsg{
				Txid:   event.Txid,
				Height: event.BlockHeight,
			}
		},
	)

	_, err = b.cfg.ChainSource.Ask(ctx, &chainsource.RegisterConfRequest{
		CallerID:    b.feeInputFanoutCallerID(txid),
		Txid:        &txid,
		PkScript:    append([]byte(nil), pkScript...),
		TargetConfs: RequiredFeeInputConfirmations,
		NotifyActor: fn.Some(notifyRef),
	}).Await(ctx).Unpack()
	if err != nil {
		return err
	}

	return nil
}

// feeInputFanoutCallerID returns the stable confirmation-watch caller ID for a
// fanout transaction in this child actor.
func (b *behavior) feeInputFanoutCallerID(txid chainhash.Hash) string {
	return b.cfg.ActorID + "-fee-input-fanout-" + txid.String()
}
