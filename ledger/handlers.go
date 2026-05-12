package ledger

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
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
			return fmt.Errorf("%w: negative amount %d",
				ErrInvalidMessage, a)
		}
	}

	return nil
}

// handleRoundConfirmed records all accounting entries for a
// confirmed round: capital deployment, boarding fees, and mining
// fees. Also updates the treasury tracker.
func (a *LedgerActor) handleRoundConfirmed(ctx context.Context,
	msg *RoundConfirmedMsg) error {

	if err := validateAmounts(
		msg.TotalVTXOAmountSat, msg.BoardingFeeSat, msg.MiningFeeSat,
		msg.BoardingNewSat, msg.RefreshNewSat,
	); err != nil {
		return err
	}

	// BoardingNewSat + RefreshNewSat must partition
	// TotalVTXOAmountSat for the double-entry ledger to balance:
	// the asset leg (RecordCapitalCommitted) moves
	// TotalVTXOAmountSat from treasury_wallet to deployed_capital,
	// and the paired liability legs (RecordBoardingDeposit +
	// RecordRefreshNewVTXO) together credit user_vtxo_claims by
	// the same total. A producer that sums the per-client splits
	// inconsistently with the VTXO-descriptor walk would break the
	// balance-sheet invariant silently; reject here instead.
	if msg.BoardingNewSat+msg.RefreshNewSat !=
		msg.TotalVTXOAmountSat {
		return fmt.Errorf("%w: RoundConfirmedMsg origin split %d + %d "+
			"!= total %d", ErrInvalidMessage, msg.BoardingNewSat,
			msg.RefreshNewSat, msg.TotalVTXOAmountSat)
	}

	roundID := msg.RoundID[:]
	now := a.clk.Now()

	a.log.InfoS(ctx, "Recording round confirmation",
		slog.String("round_id",
			fmt.Sprintf("%x", roundID)),
		slog.Int64("vtxo_total_sat",
			msg.TotalVTXOAmountSat),
		slog.Int64("boarding_new_sat", msg.BoardingNewSat),
		slog.Int64("refresh_new_sat", msg.RefreshNewSat),
		slog.Int64("boarding_fee_sat",
			msg.BoardingFeeSat),
		slog.Int64("mining_fee_sat", msg.MiningFeeSat),
		slog.Uint64("block_height",
			uint64(msg.BlockHeight)),
	)

	// Record capital committed to fund new VTXOs in the round.
	// This is the asset-side move: treasury_wallet -> deployed_capital.
	if msg.TotalVTXOAmountSat > 0 {
		err := fees.RecordCapitalCommitted(
			ctx, a.cfg.LedgerStore, roundID,
			btcutil.Amount(msg.TotalVTXOAmountSat), now,
		)
		if err != nil {
			return fmt.Errorf("record capital committed: %w", err)
		}
	}

	// Record the liability side for new user VTXO claims minted
	// from boarding inputs: deployed_capital -> user_vtxo_claims.
	// Without this leg, deployed_capital grows every round while
	// user_vtxo_claims stays at zero, so the double-entry balance
	// sheet is silently unbalanced. Zero on refresh-only rounds.
	if msg.BoardingNewSat > 0 {
		err := fees.RecordBoardingDeposit(
			ctx, a.cfg.LedgerStore, roundID,
			btcutil.Amount(msg.BoardingNewSat), now,
		)
		if err != nil {
			return fmt.Errorf("record boarding deposit: %w", err)
		}
	}

	// Record the liability side for new user VTXO claims minted
	// from refresh (forfeit) clients:
	// deployed_capital -> user_vtxo_claims. The paired retirement
	// leg for the old VTXO claim is booked by handleVTXOsForfeited
	// when the VTXOsForfeitedMsg arrives for this same round.
	if msg.RefreshNewSat > 0 {
		err := fees.RecordRefreshNewVTXO(
			ctx, a.cfg.LedgerStore, roundID,
			btcutil.Amount(msg.RefreshNewSat), now,
		)
		if err != nil {
			return fmt.Errorf("record refresh new vtxo: %w", err)
		}
	}

	// Record boarding fees collected.
	if msg.BoardingFeeSat > 0 {
		err := fees.RecordBoardingFee(
			ctx, a.cfg.LedgerStore, roundID,
			btcutil.Amount(msg.BoardingFeeSat), now,
		)
		if err != nil {
			return fmt.Errorf("record boarding fee: %w", err)
		}
	}

	// Record mining fees paid.
	if msg.MiningFeeSat > 0 {
		err := fees.RecordMiningFee(
			ctx, a.cfg.LedgerStore, roundID,
			btcutil.Amount(msg.MiningFeeSat), now,
		)
		if err != nil {
			return fmt.Errorf("record mining fee: %w", err)
		}
	}

	// Pre-insert attribution rows for the round's funding
	// inputs and change outputs. The UTXO diff loop short-
	// circuits on these (the UNIQUE (hash, index, event)
	// constraint turns its pending insert into a no-op) so it
	// does NOT book external_withdrawal / external_deposit
	// for round-attributable movements.
	if err := a.preInsertAttribution(
		ctx, msg.FundingOutpoints, UTXOAuditSpent,
		UTXOClassRoundFunding, msg.RoundID[:], int64(msg.BlockHeight),
	); err != nil {
		return fmt.Errorf("pre-insert funding attribution: %w", err)
	}
	if err := a.preInsertAttribution(
		ctx, msg.ChangeOutpoints, UTXOAuditCreated,
		UTXOClassRoundChange, msg.RoundID[:], int64(msg.BlockHeight),
	); err != nil {
		return fmt.Errorf("pre-insert change attribution: %w", err)
	}

	// Update treasury tracker.
	if a.cfg.TreasuryTracker != nil {
		a.cfg.TreasuryTracker.OnRoundConfirmed(
			msg.TotalVTXOAmountSat, int(msg.VTXOCount),
		)
	}

	return nil
}

// preInsertAttribution writes audit rows for a producer-
// attributed set of outpoints before the next BlockEpochMsg
// drains from the mailbox, so the diff loop's pending insert
// on the same (outpoint, event) key is a silent no-op.
//
// Amount is left at zero because the pre-insert does not know
// the wallet's live balance for each outpoint; the diff loop
// does. That's fine: the row's purpose is attribution, and
// the amount column is informational for operator reports
// only (the ledger legs that actually move money are booked
// separately by the handler). A follow-up can thread exact
// amounts through the TLV payload if tax-side reporting
// needs them.
func (a *LedgerActor) preInsertAttribution(
	ctx context.Context, ops []wire.OutPoint,
	event UTXOAuditEvent, class UTXOClassification,
	sourceID []byte, blockHeight int64,
) error {

	if len(ops) == 0 {
		return nil
	}
	if !a.cfg.UTXOAuditStore.IsSome() {
		return nil
	}

	now := a.clk.Now()
	for _, op := range ops {
		if _, err := a.writeAuditRow(
			ctx, WalletUTXOLogEntry{
				Outpoint:       op,
				Event:          event,
				BlockHeight:    blockHeight,
				Classification: class,
				CreatedAt:      now,
				SourceID:       sourceID,
			},
		); err != nil {
			return err
		}
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
func (a *LedgerActor) handleVTXOsForfeited(ctx context.Context,
	msg *VTXOsForfeitedMsg) error {

	if err := validateAmounts(
		msg.TotalAmountSat, msg.RefreshFeeSat,
	); err != nil {
		return err
	}

	roundID := msg.RoundID[:]
	now := a.clk.Now()

	a.log.InfoS(ctx, "Recording VTXO forfeit",
		slog.String("round_id",
			fmt.Sprintf("%x", roundID)),
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
			return fmt.Errorf("record refresh forfeit: %w", err)
		}
	}

	// Record refresh fee if any was collected.
	if msg.RefreshFeeSat > 0 {
		err := fees.RecordRefreshFee(
			ctx, a.cfg.LedgerStore, roundID,
			btcutil.Amount(msg.RefreshFeeSat), now,
		)
		if err != nil {
			return fmt.Errorf("record refresh fee: %w", err)
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
func (a *LedgerActor) handleSweepCompleted(ctx context.Context,
	msg *SweepCompletedMsg) error {

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
			return fmt.Errorf("record round sweep: %w", err)
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
			return fmt.Errorf("record sweep mining fee: %w", err)
		}
	}

	// Pre-insert attribution rows for the sweep's consumed
	// inputs and return outputs so the UTXO diff loop does
	// not double-book external_* legs for sweep-attributable
	// movements.
	if err := a.preInsertAttribution(
		ctx, msg.ConsumedOutpoints, UTXOAuditSpent,
		UTXOClassSweepConsumption, msg.BatchID[:],
		int64(msg.BlockHeight),
	); err != nil {
		return fmt.Errorf("pre-insert sweep consumption "+
			"attribution: %w", err)
	}
	if err := a.preInsertAttribution(
		ctx, msg.ReturnOutpoints, UTXOAuditCreated,
		UTXOClassSweepReturn, msg.BatchID[:], int64(msg.BlockHeight),
	); err != nil {
		return fmt.Errorf("pre-insert sweep return attribution: %w",
			err)
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
func (a *LedgerActor) handleOORFinalized(ctx context.Context,
	msg *OORFinalizedMsg) error {

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

// handleBlockEpoch processes a new block notification. It runs
// two passes:
//
//  1. Reconciliation: promote any 'pending' audit row left
//     behind by the previous block's diff to its terminal
//     classification and book the matching external_* ledger
//     leg. A one-block grace window lets the producer's
//     RoundConfirmedMsg / SweepCompletedMsg land on the
//     mailbox and attribute the outpoint before the
//     classifier concludes the movement is genuinely external.
//  2. Diff: compare the treasury wallet's current UTXO set
//     against the actor's previous snapshot, insert audit
//     rows with classified_as='pending' for each change
//     (already-attributed rows from handler pre-inserts are
//     silent no-ops via UNIQUE(hash, index, event)).
//
// When the lister is None, both passes degrade to log-only
// no-ops.
func (a *LedgerActor) handleBlockEpoch(ctx context.Context,
	msg *BlockEpochMsg) error {

	a.log.DebugS(ctx, "Block epoch received",
		slog.Uint64("height", uint64(msg.BlockHeight)),
	)

	if err := a.reconcilePendingAuditRows(
		ctx, int64(msg.BlockHeight),
	); err != nil {
		return err
	}

	return a.processBlockUTXODiff(ctx, int64(msg.BlockHeight))
}
