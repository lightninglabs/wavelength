package ledger

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/darepo/fees"
)

// validateAmounts rejects negative values up front so a
// malformed TLV dead-letters via ErrInvalidMessage instead of
// driving infinite nack-and-retry against the SQL
// `CHECK (amount_sat > 0)` constraint. Zero is allowed here
// because handlers independently skip zero-fee legs; the
// rejection is for definitely-wrong negatives.
func validateAmounts(amounts ...int64) error {
	for _, a := range amounts {
		if a < 0 {
			return fmt.Errorf(
				"%w: negative amount %d",
				ErrInvalidMessage, a,
			)
		}
	}

	return nil
}

// handleRoundConfirmed records all accounting entries for a
// confirmed round: capital deployment, boarding fees, and mining
// fees. Also updates the treasury tracker.
func (a *LedgerActor) handleRoundConfirmed(
	ctx context.Context, msg *RoundConfirmedMsg) error {

	if err := validateAmounts(
		msg.TotalVTXOAmountSat,
		msg.BoardingFeeSat,
		msg.MiningFeeSat,
	); err != nil {
		return err
	}

	roundID := msg.RoundID[:]
	now := a.clk.Now()

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
			btcutil.Amount(msg.TotalVTXOAmountSat), now,
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
			btcutil.Amount(msg.BoardingFeeSat), now,
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
			btcutil.Amount(msg.MiningFeeSat), now,
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

// handleVTXOsForfeited records the capital-retirement leg and
// the refresh fee when VTXOs are forfeited, then updates the
// treasury tracker.
//
// The forfeit books two ledger legs. The retirement leg debits
// user_vtxo_claims and credits deployed_capital for the gross
// forfeited amount, releasing the user's outstanding claim back
// to the deployed-capital pool. The fee leg (when
// RefreshFeeSat > 0) debits user_vtxo_claims and credits
// refresh_fee_revenue for the operator's share of the refresh.
// Both legs share the same round_id; the partial unique index
// discriminates on event_type so the shared idempotency key
// does not collide.
//
// Tracker update stays LAST so a mid-handler DB failure does not
// advance the in-memory state ahead of the persisted ledger --
// the idempotency invariant from H-1 depends on this ordering.
func (a *LedgerActor) handleVTXOsForfeited(
	ctx context.Context, msg *VTXOsForfeitedMsg) error {

	if err := validateAmounts(
		msg.TotalAmountSat, msg.RefreshFeeSat,
	); err != nil {
		return err
	}

	roundID := msg.RoundID[:]
	now := a.clk.Now()

	a.log.InfoS(ctx, "Recording VTXO forfeit",
		slog.String("round_id",
			fmt.Sprintf("%x", msg.RoundID)),
		slog.Int64("total_sat", msg.TotalAmountSat),
		slog.Int("count", int(msg.Count)),
	)

	// Retire the old user VTXO claim: the gross forfeited
	// amount moves from user_vtxo_claims back into
	// deployed_capital. This is the missing balance-sheet leg
	// that lets user_vtxo_claims converge to the actual
	// outstanding user obligation instead of drifting upward
	// each round.
	if msg.TotalAmountSat > 0 {
		err := fees.RecordRefreshForfeit(
			ctx, a.cfg.LedgerStore, roundID,
			btcutil.Amount(msg.TotalAmountSat), now,
		)
		if err != nil {
			return fmt.Errorf("record refresh "+
				"forfeit: %w", err)
		}
	}

	// Record refresh fee if any was collected.
	if msg.RefreshFeeSat > 0 {
		err := fees.RecordRefreshFee(
			ctx, a.cfg.LedgerStore, roundID,
			btcutil.Amount(msg.RefreshFeeSat), now,
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

// handleSweepCompleted records capital reclamation and the
// on-chain mining fee when expired VTXOs are swept back into
// the operator wallet.
//
// The sweep books two legs: the reclaim leg (round_sweep, debit
// treasury_wallet, credit deployed_capital) for the amount
// returned to the operator, and the mining-fee leg (mining_fee,
// debit mining_fees, credit treasury_wallet) for the on-chain
// cost of the sweep transaction. Booking the mining fee here is
// what keeps the ledger's treasury_wallet balance converging to
// on-chain reality -- without this leg the operator's expense
// account silently drifts behind the actual mining spend.
//
// Both legs share BatchID as the idempotency key; the partial
// unique index discriminates on event_type so the shared key
// does not collide. Tracker update stays LAST per the H-1
// invariant.
func (a *LedgerActor) handleSweepCompleted(
	ctx context.Context, msg *SweepCompletedMsg) error {

	if err := validateAmounts(
		msg.ReclaimedAmountSat, msg.MiningFeeSat,
	); err != nil {
		return err
	}

	a.log.InfoS(ctx, "Recording sweep completion",
		slog.String("batch_id",
			fmt.Sprintf("%x", msg.BatchID)),
		slog.Int64("reclaimed_sat",
			msg.ReclaimedAmountSat),
		slog.Int64("mining_fee_sat", msg.MiningFeeSat),
		slog.Int("count", int(msg.Count)),
		slog.Uint64("block_height",
			uint64(msg.BlockHeight)),
	)

	batchID := msg.BatchID[:]
	now := a.clk.Now()

	if msg.ReclaimedAmountSat > 0 {
		// Use BatchID as the round identifier for the
		// sweep. RoundSweep is logged against the batch
		// that retired the expired VTXOs.
		err := fees.RecordRoundSweep(
			ctx, a.cfg.LedgerStore, batchID,
			btcutil.Amount(msg.ReclaimedAmountSat), now,
		)
		if err != nil {
			return fmt.Errorf("record round sweep: %w",
				err)
		}
	}

	// Book the on-chain cost of the sweep tx as a mining-fee
	// expense against the treasury wallet. Producers that do
	// not yet capture the absolute fee (pre-wiring path) pass
	// zero and this leg is silently skipped.
	if msg.MiningFeeSat > 0 {
		err := fees.RecordMiningFee(
			ctx, a.cfg.LedgerStore, batchID,
			btcutil.Amount(msg.MiningFeeSat), now,
		)
		if err != nil {
			return fmt.Errorf("record sweep "+
				"mining fee: %w", err)
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

	if err := validateAmounts(
		msg.InputAmountSat, msg.OutputAmountSat,
	); err != nil {
		return err
	}

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

	err := fees.RecordOORTransfer(
		ctx, a.cfg.LedgerStore, msg.SessionID[:],
		btcutil.Amount(feeSat), a.clk.Now(),
	)
	if err != nil {
		return fmt.Errorf("record OOR transfer: %w", err)
	}

	return nil
}

// handleBlockEpoch processes a new block notification. When
// the wallet UTXO lister is configured, it triggers the diff
// subsystem that compares the treasury wallet's current UTXO
// set against the actor's previous snapshot, writes audit
// rows for every movement, and books external_deposit /
// external_withdrawal ledger entries for unclassified changes.
// When the lister is None, this is a log-only no-op.
func (a *LedgerActor) handleBlockEpoch(
	ctx context.Context, msg *BlockEpochMsg) error {

	a.log.DebugS(ctx, "Block epoch received",
		slog.Uint64("height", uint64(msg.BlockHeight)),
	)

	return a.processBlockUTXODiff(ctx, int64(msg.BlockHeight))
}
