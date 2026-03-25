package ledgeractor

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/lightninglabs/darepo/fees"
)

// timeNowUnix returns the current time as a Unix timestamp.
func timeNowUnix() int64 {
	return time.Now().Unix()
}

// handleRoundConfirmed records all accounting entries for a
// confirmed round: capital deployment, boarding fees, and mining
// fees. Also updates the treasury tracker.
func (a *LedgerActor) handleRoundConfirmed(
	ctx context.Context, msg *RoundConfirmedMsg) error {

	roundID := msg.RoundID[:]

	a.log.InfoS(ctx, "Recording round confirmation",
		slog.String("round_id",
			fmt.Sprintf("%x", msg.RoundID)),
		slog.Int64("vtxo_total_sat",
			msg.TotalVTXOAmountSat),
		slog.Int64("boarding_fee_sat",
			msg.BoardingFeeSat),
		slog.Int64("mining_fee_sat", msg.MiningFeeSat),
		slog.Uint64("block_height",
			uint64(msg.BlockHeight)),
	)

	// Record capital deployed to VTXOs.
	if msg.TotalVTXOAmountSat > 0 {
		err := fees.RecordCapitalDeployment(
			ctx, a.cfg.LedgerStore, roundID,
			msg.TotalVTXOAmountSat,
		)
		if err != nil {
			return fmt.Errorf("record capital "+
				"deployment: %w", err)
		}
	}

	// Record boarding fees collected.
	if msg.BoardingFeeSat > 0 {
		err := fees.RecordBoardingFee(
			ctx, a.cfg.LedgerStore, roundID,
			msg.BoardingFeeSat,
		)
		if err != nil {
			return fmt.Errorf("record boarding "+
				"fee: %w", err)
		}
	}

	// Record mining fees paid.
	if msg.MiningFeeSat > 0 {
		err := fees.RecordMiningFee(
			ctx, a.cfg.LedgerStore, roundID,
			msg.MiningFeeSat,
		)
		if err != nil {
			return fmt.Errorf("record mining "+
				"fee: %w", err)
		}
	}

	// Update treasury tracker.
	if a.cfg.TreasuryTracker != nil {
		a.cfg.TreasuryTracker.OnRoundConfirmed(
			msg.TotalVTXOAmountSat,
			int(msg.VTXOCount),
		)
	}

	return nil
}

// handleVTXOsForfeited records refresh fees and updates the
// treasury tracker when VTXOs are forfeited.
func (a *LedgerActor) handleVTXOsForfeited(
	ctx context.Context, msg *VTXOsForfeitedMsg) error {

	roundID := msg.RoundID[:]

	a.log.InfoS(ctx, "Recording VTXO forfeit",
		slog.String("round_id",
			fmt.Sprintf("%x", msg.RoundID)),
		slog.Int64("total_sat", msg.TotalAmountSat),
		slog.Int("count", int(msg.Count)),
	)

	// Record refresh fee if any was collected.
	if msg.RefreshFeeSat > 0 {
		err := fees.RecordRefreshFee(
			ctx, a.cfg.LedgerStore, roundID,
			msg.RefreshFeeSat,
		)
		if err != nil {
			return fmt.Errorf("record refresh "+
				"fee: %w", err)
		}
	}

	// Update treasury tracker.
	if a.cfg.TreasuryTracker != nil {
		a.cfg.TreasuryTracker.OnVTXOsForfeited(
			msg.TotalAmountSat, int(msg.Count),
		)
	}

	return nil
}

// handleSweepCompleted records capital reclamation when expired
// VTXOs are swept back into the operator wallet.
func (a *LedgerActor) handleSweepCompleted(
	ctx context.Context, msg *SweepCompletedMsg) error {

	a.log.InfoS(ctx, "Recording sweep completion",
		slog.String("batch_id",
			fmt.Sprintf("%x", msg.BatchID)),
		slog.Int64("reclaimed_sat",
			msg.ReclaimedAmountSat),
		slog.Int("count", int(msg.Count)),
		slog.Uint64("block_height",
			uint64(msg.BlockHeight)),
	)

	if msg.ReclaimedAmountSat > 0 {
		err := fees.RecordCapitalReclaimed(
			ctx, a.cfg.LedgerStore,
			msg.ReclaimedAmountSat,
		)
		if err != nil {
			return fmt.Errorf("record capital "+
				"reclaimed: %w", err)
		}
	}

	// Update treasury tracker.
	if a.cfg.TreasuryTracker != nil {
		a.cfg.TreasuryTracker.OnSweepCompleted(
			msg.ReclaimedAmountSat, int(msg.Count),
		)
	}

	return nil
}

// handleOORFinalized records OOR transfer volume. OOR transfers
// are free (no fee) but tracked for audit purposes. When input
// and output amounts are provided, a volume memo entry is
// recorded. When amounts are zero (session-only tracking), only
// a debug log is emitted.
func (a *LedgerActor) handleOORFinalized(
	ctx context.Context, msg *OORFinalizedMsg) error {

	a.log.DebugS(ctx, "Recording OOR transfer",
		slog.String("session_id",
			fmt.Sprintf("%x", msg.SessionID)),
		slog.Int64("input_sat", msg.InputAmountSat),
		slog.Int64("output_sat", msg.OutputAmountSat),
	)

	// OOR transfers are free but we record volume for
	// audit when amounts are available. The debit and
	// credit go to the same account (deployed_capital)
	// making this a memo/volume entry that doesn't change
	// balances.
	if msg.InputAmountSat > 0 {
		err := a.cfg.LedgerStore.InsertLedgerEntry(
			ctx, fees.LedgerEntry{
				DebitAccount: fees.AccountDeployedCapital,
				CreditAccount: fees.AccountDeployedCapital,
				AmountSat:    msg.InputAmountSat,
				EventType: string(
					fees.LedgerEventOORTransfer,
				),
				Description: fmt.Sprintf(
					"OOR transfer volume: "+
						"%d→%d sats",
					msg.InputAmountSat,
					msg.OutputAmountSat,
				),
				CreatedAt: timeNowUnix(),
			},
		)
		if err != nil {
			return fmt.Errorf("record OOR volume: %w",
				err)
		}
	}

	return nil
}

// handleBlockEpoch processes a new block notification. It
// updates the treasury wallet balance. UTXO diffing is handled
// by the utxo_differ component if configured.
func (a *LedgerActor) handleBlockEpoch(
	ctx context.Context, msg *BlockEpochMsg) error {

	a.log.DebugS(ctx, "Block epoch received",
		slog.Uint64("height", uint64(msg.BlockHeight)),
	)

	// TODO(fees): implement UTXO set diffing here once
	// WalletController UTXO listing is wired.

	return nil
}
