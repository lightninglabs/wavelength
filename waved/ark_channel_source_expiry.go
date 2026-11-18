package waved

import (
	"context"
	"fmt"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/arkchannel"
	"github.com/lightninglabs/wavelength/chainsource"
	"github.com/lightningnetwork/lnd/clock"
)

// runSourceExpiryGuard reconciles the current tip after registration and every
// wakeup. A closed or overflowing block stream is re-registered, so missed
// heights and restarts cannot move an authenticated recovery deadline.
func (c *NativeArkChannelController) runSourceExpiryGuard(ctx context.Context,
	service *arkchannel.Service, node arkChannelForceCloseResumer) {

	protect := func(height int32) {
		err := c.protectExpiringSources(ctx, service, node, height)
		_ = tolerateNativeArkChannelFailures(
			ctx, err, c.cfg.Log,
			"Ark channel source expiry protection failed",
		)
	}
	if tip := c.cfg.SourceExpiryTip; tip != nil {
		for ctx.Err() == nil {
			height, next := tip()
			if height >= 0 {
				protect(height)
			}
			select {
			case <-ctx.Done():
				return

			case <-next:
			}
		}

		return
	}
	watchSourceExpiryTip(
		ctx, c.cfg.ChainBackend, c.cfg.Clock, c.cfg.Log, protect,
	)
}

// watchSourceExpiryTip reads the authoritative height on registration and
// wakeup. A dropped notification is not proof that the deadline is distant.
func watchSourceExpiryTip(ctx context.Context, backend chainsource.ChainBackend,
	clk clock.Clock, log btclog.Logger, notify func(int32)) {

	refresh := func() {
		height, _, err := backend.BestBlock(ctx)
		if err == nil {
			notify(height)
		}
	}
	warned := false
	for ctx.Err() == nil {
		registration, err := backend.RegisterBlocks(ctx)
		if err != nil && !warned && log != nil {
			log.WarnS(ctx, "Ark channel expiry watch unavailable",
				err,
			)
			warned = true
		}
		if err != nil {
			// A broken notification stream need not imply a broken
			// tip query. Reconcile even while re-registering.
			refresh()
		} else {
			warned = false
			for ctx.Err() == nil {
				refresh()
				select {
				case <-ctx.Done():
					registration.Cancel()

					return

				case _, ok := <-registration.Epochs:
					if !ok {
						registration.Cancel()
						goto retry
					}

				case <-clk.TickAfter(
					defaultArkChannelPrePONRScanInterval,
				):
					// Retry store/backend failures even if
					// no new block follows the threshold
					// crossing.
				}
			}
			registration.Cancel()
		}

	retry:
		select {
		case <-ctx.Done():
			return

		case <-clk.TickAfter(
			defaultArkChannelClientRetryInterval,
		):
		}
	}
}

// protectExpiringSources persists each handoff before starting its existing
// replay worker. Waiting for CSV or CPFP never blocks another channel's guard.
func (c *NativeArkChannelController) protectExpiringSources(ctx context.Context,
	service *arkchannel.Service, node arkChannelForceCloseResumer,
	height int32) error {

	records, err := service.ListChannels(ctx)
	if err != nil {
		return err
	}
	failures := make([]arkchannel.ResumeFailure, 0)
	for _, record := range records {
		snapshot := record.Snapshot
		//nolint:contextcheck // controller-owned observation
		c.watchExpiringPayout(service, snapshot)
		if !needsSourceExpiryProtection(snapshot) {
			continue
		}
		expiring, err := c.cfg.Recovery.SourceExpiring(
			ctx, snapshot.Terms, *snapshot.Source, height,
		)
		if err == nil && expiring {
			var protected arkchannel.Record
			protected, _, err = c.coordinator.Apply(
				ctx, snapshot.Terms.ID,
				&arkchannel.SourceExpiring{},
			)
			if err == nil {
				//nolint:contextcheck // owned
				c.watchExpiringPayout(
					service, protected.Snapshot,
				)
				//nolint:contextcheck // owned
				c.startNativeArkChannelResume(
					service, node,
					[]arkchannel.ID{snapshot.Terms.ID},
				)
			}
		}
		if err != nil {
			failures = append(failures, arkchannel.ResumeFailure{
				ChannelID: snapshot.Terms.ID,
				Err: fmt.Errorf("protect source expiry: "+
					"%w", err),
			})
		}
	}
	if len(failures) != 0 {
		return &arkchannel.ResumeFailures{Failures: failures}
	}

	return nil
}

// needsSourceExpiryProtection excludes completed, already-published, and
// already-handed-off channels. Their existing recovery worker owns retries.
func needsSourceExpiryProtection(snapshot arkchannel.Snapshot) bool {
	if snapshot.Source == nil || !snapshot.OORFinalized ||
		(!snapshot.ClientRecoveryReady && !snapshot.HubRecoveryReady) {
		return false
	}
	switch snapshot.Phase {
	case arkchannel.PhaseBackingReady, arkchannel.PhaseActivating,
		arkchannel.PhaseActive, arkchannel.PhaseCoopClosing,
		arkchannel.PhaseCoopCloseSigned:
		return true

	default:
		return false
	}
}

// watchExpiringPayout retains both resolution candidates after expiry wins
// the phase race. Its in-memory deduplication is rebuilt from durable state on
// restart; it never authorizes a payout or consumes a pending control request.
func (c *NativeArkChannelController) watchExpiringPayout(
	service *arkchannel.Service, snapshot arkchannel.Snapshot) {

	if snapshot.CooperativeClose == nil ||
		(snapshot.Phase != arkchannel.PhaseMaterializing &&
			snapshot.Phase != arkchannel.PhaseOnChain) {
		return
	}
	if _, exists := c.expiryPayoutWatches.LoadOrStore(
		snapshot.Terms.ID, struct{}{},
	); exists {
		return
	}
	c.startLifecycleWorker(func(ctx context.Context) {
		defer c.expiryPayoutWatches.Delete(snapshot.Terms.ID)
		c.observeExpiringPayout(ctx, service, snapshot)
	})
}

// observeExpiringPayout accepts only local confirmation of the exact agreed
// transaction. A peer's OOR-finalized receipt cannot prove a competing backing
// lost and therefore cannot retire native watchers after expiry protection.
func (c *NativeArkChannelController) observeExpiringPayout(ctx context.Context,
	service *arkchannel.Service, snapshot arkchannel.Snapshot) {

	settlement := snapshot.CooperativeClose
	template, err := arkchannel.NewCooperativeCloseTemplate(
		snapshot.Terms, *snapshot.Source,
		*snapshot.CooperativeCloseRequest,
		settlement.Proposal.ClientBalance,
		settlement.Proposal.HubBalance,
		settlement.Proposal.CommitmentHeight,
	)
	if err != nil {
		return
	}
	script := template.ArkTransaction().TxOut[0].PkScript
	for ctx.Err() == nil {
		registration, err := c.cfg.ChainBackend.RegisterConf(
			ctx, &settlement.TxID, script, 1, 0, false,
		)
		if err == nil {
			select {
			case confirmation, ok := <-registration.Confirmed:
				event := &arkchannel.CooperativeCloseObserved{
					TxID: settlement.TxID,
				}
				valid := ok && confirmation != nil &&
					confirmation.Tx != nil &&
					confirmation.BlockHeight > 0
				if valid {
					valid = confirmation.Tx.TxHash() ==
						settlement.TxID
				}
				if valid {
					_, err = service.ApplyLocalEvent(
						ctx, snapshot.Terms.ID, event,
					)
					registration.Cancel()
					if err == nil {
						return
					}
				} else {
					registration.Cancel()
				}

			case <-ctx.Done():
				registration.Cancel()

				return

			case <-c.cfg.Clock.TickAfter(
				defaultArkChannelPrePONRScanInterval,
			):
				registration.Cancel()
				record, err := service.GetChannel(
					ctx, snapshot.Terms.ID,
				)
				phase := record.Snapshot.Phase
				settled := record.Snapshot.CooperativeClose ==
					nil
				resolved := settled ||
					phase == arkchannel.PhaseClosed
				if err == nil && resolved {
					return
				}
			}
		}
		select {
		case <-ctx.Done():
			return

		case <-c.cfg.Clock.TickAfter(
			defaultArkChannelClientRetryInterval,
		):
		}
	}
}
