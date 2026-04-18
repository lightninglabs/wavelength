package ledger

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/lightninglabs/darepo/fees"
)

// handleRoundConfirmed records all accounting entries for a
// confirmed round: capital deployment, boarding fees, and mining
// fees. Also updates the treasury tracker.
func (a *LedgerActor) handleRoundConfirmed(
	ctx context.Context, msg *RoundConfirmedMsg) error {

	roundID := msg.RoundID[:]
	now := time.Now()

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

	// Record capital committed to fund new VTXOs in the round.
	if msg.TotalVTXOAmountSat > 0 {
		err := fees.RecordCapitalCommitted(
			ctx, a.cfg.LedgerStore, roundID,
			msg.TotalVTXOAmountSat, now,
		)
		if err != nil {
			return fmt.Errorf("record capital "+
				"committed: %w", err)
		}
	}

	// Record boarding fees collected.
	if msg.BoardingFeeSat > 0 {
		err := fees.RecordBoardingFee(
			ctx, a.cfg.LedgerStore, roundID,
			msg.BoardingFeeSat, now,
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
			msg.MiningFeeSat, now,
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
	now := time.Now()

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
			msg.RefreshFeeSat, now,
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
		// Use BatchID as the round identifier for the
		// sweep. RoundSweep is logged against the batch
		// that retired the expired VTXOs.
		err := fees.RecordRoundSweep(
			ctx, a.cfg.LedgerStore, msg.BatchID[:],
			msg.ReclaimedAmountSat, time.Now(),
		)
		if err != nil {
			return fmt.Errorf("record round sweep: %w",
				err)
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

// handleOORFinalized records an OOR transfer fee when the
// input/output delta is positive. OOR is free today
// (input == output so fee == 0 and the insert is skipped),
// but the call path is wired so that once OOR fees are
// introduced the event lands in the ledger alongside other
// fee events.
func (a *LedgerActor) handleOORFinalized(
	ctx context.Context, msg *OORFinalizedMsg) error {

	a.log.DebugS(ctx, "OOR transfer finalized",
		slog.String("session_id",
			fmt.Sprintf("%x", msg.SessionID)),
		slog.Int64("input_sat", msg.InputAmountSat),
		slog.Int64("output_sat", msg.OutputAmountSat),
	)

	feeSat := msg.InputAmountSat - msg.OutputAmountSat
	if feeSat <= 0 {
		return nil
	}

	// SessionID is 32 bytes; the ledger's round_id column is
	// a variable-width BYTEA, so we pass the full identifier
	// for correlation without truncation.
	err := fees.RecordOORTransfer(
		ctx, a.cfg.LedgerStore, msg.SessionID[:],
		feeSat, time.Now(),
	)
	if err != nil {
		return fmt.Errorf("record OOR transfer: %w", err)
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
