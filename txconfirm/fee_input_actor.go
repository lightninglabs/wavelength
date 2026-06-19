package txconfirm

import (
	"context"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightninglabs/darepo-client/chainsource"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

func (a *TxBroadcasterActor) maybeEnsureFeeInputSupply(ctx context.Context,
	err error) {

	if !errors.Is(err, ErrCPFPFeeInputUnavailable) {
		return
	}
	if !errors.Is(err, errCPFPFeeInputShortfall) {
		return
	}

	demands, feeRate, err := a.activeFeeInputDemands(ctx)
	if err != nil {
		a.log.WarnS(ctx, "Unable to compute fee-input demand",
			err)

		return
	}

	pending, err := a.feeBumpController.EnsureSupply(
		ctx, demands, feeRate, a.bestHeight,
		a.cfg.FeeBumpIntervalBlocks,
	)
	if err != nil {
		a.log.WarnS(ctx, "Unable to fan out fee inputs", err)

		return
	}
	if pending == nil {
		return
	}

	if err := a.registerFanoutConfWatch(ctx, pending); err != nil {
		a.log.WarnS(ctx, "Unable to watch fee-input fanout",
			err, "txid", pending.txid)
	}
}

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

		if state == TxStateConfirmed || state == TxStateFailed {
			continue
		}

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

func (a *TxBroadcasterActor) registerFanoutConfWatch(ctx context.Context,
	pending *pendingFeeInputFanout) error {

	if pending == nil || len(pending.watchScript) == 0 {
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

	txid := pending.txid
	watchScript := append([]byte(nil), pending.watchScript...)
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

func (a *TxBroadcasterActor) unregisterFanoutConfWatch(ctx context.Context,
	pending *pendingFeeInputFanout) error {

	if pending == nil || len(pending.watchScript) == 0 {
		return nil
	}

	txid := pending.txid
	watchScript := append([]byte(nil), pending.watchScript...)
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

func (a *TxBroadcasterActor) handleFanoutConfirmed(ctx context.Context,
	msg *confirmationObservedMsg) bool {

	pending := a.feeBumpController.PendingFanout()
	if pending == nil || pending.txid != msg.txid {
		return false
	}

	if err := a.unregisterFanoutConfWatch(ctx, pending); err != nil {
		a.log.WarnS(ctx, "Failed to unregister fanout watch",
			err, "txid", pending.txid)
	}

	a.feeBumpController.OnFanoutConfirmed(ctx, msg.txid)
	a.retryBroadcastingParents(ctx)

	return true
}

func (a *TxBroadcasterActor) retryBroadcastingParents(ctx context.Context) {
	for _, entry := range a.tracked {
		state, err := entry.currentTxState()
		if err != nil || state != TxStateBroadcasting {
			continue
		}

		err = a.broadcastTrackedTx(ctx, entry, TxStateBroadcasting)
		a.recordInitialBroadcastOutcome(ctx, entry, err)
	}
}

func (a *TxBroadcasterActor) fanoutConfCallerID(txid chainhash.Hash) string {
	return "txconfirm-fee-input-fanout-" + txid.String()
}
