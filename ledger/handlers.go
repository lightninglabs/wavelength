package ledger

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// zeroRoundID is the zero value used to detect empty round IDs.
var zeroRoundID [16]byte

// zeroSessionID is the zero value used to detect empty session IDs.
var zeroSessionID [32]byte

// roundIDOrNil converts a 16-byte round ID to a slice, returning
// nil for zero-valued IDs so the database stores NULL (which
// correctly bypasses the conditional idempotency unique index).
func roundIDOrNil(id [16]byte) []byte {
	if id == zeroRoundID {
		return nil
	}

	return id[:]
}

// sessionIDOrNil converts a 32-byte session ID to a slice,
// returning nil for zero-valued IDs so the database stores NULL
// (which correctly bypasses the conditional idempotency unique
// index on session_id).
func sessionIDOrNil(id [32]byte) []byte {
	if id == zeroSessionID {
		return nil
	}

	return id[:]
}

// timeNowUnix returns the current time as a Unix timestamp.
func timeNowUnix() int64 {
	return time.Now().Unix()
}

// handleFeePaid records a fee payment by the client. Fees paid
// during boarding or refresh are debited from fees_paid and
// credited to vtxo_balance (the fee reduces the client's VTXO
// balance).
func (a *LedgerActor) handleFeePaid(
	ctx context.Context, msg *FeePaidMsg) error {

	roundID := roundIDOrNil(msg.RoundID)

	// Map the fee type string to the appropriate event type.
	var eventType string
	switch msg.FeeType {
	case FeeTypeBoarding:
		eventType = EventBoardingFeePaid
	case FeeTypeRefresh:
		eventType = EventRefreshFeePaid
	default:
		return fmt.Errorf("unknown fee type: %q", msg.FeeType)
	}

	a.log.InfoS(ctx, "Recording fee payment",
		slog.String("round_id",
			fmt.Sprintf("%x", msg.RoundID)),
		slog.Int64("amount_sat", msg.AmountSat),
		slog.String("fee_type", msg.FeeType),
		slog.Uint64("block_height",
			uint64(msg.BlockHeight)),
	)

	return a.cfg.LedgerStore.InsertLedgerEntry(
		ctx, LedgerEntry{
			DebitAccount:  AccountFeesPaid,
			CreditAccount: AccountVTXOBalance,
			AmountSat:     msg.AmountSat,
			RoundID:       roundID,
			EventType:     eventType,
			Description: fmt.Sprintf(
				"%s fee paid in round %x",
				msg.FeeType, msg.RoundID,
			),
			CreatedAt: timeNowUnix(),
		},
	)
}

// handleVTXOReceived records a VTXO received by the client.
// For OOR transfers, the counterparty side is booked to
// transfers_in (debit vtxo_balance, credit transfers_in). For
// round receipts, the balance moves from wallet_balance to
// vtxo_balance.
func (a *LedgerActor) handleVTXOReceived(
	ctx context.Context, msg *VTXOReceivedMsg) error {

	roundID := roundIDOrNil(msg.RoundID)

	a.log.InfoS(ctx, "Recording VTXO received",
		slog.String("outpoint",
			fmt.Sprintf("%x:%d",
				msg.OutpointHash,
				msg.OutpointIndex)),
		slog.Int64("amount_sat", msg.AmountSat),
		slog.String("source", msg.Source),
	)

	var (
		debitAccount  string
		creditAccount string
	)

	switch msg.Source {
	case SourceOOR:
		// OOR receive from another participant: counterparty
		// side is transfers_in.
		debitAccount = AccountVTXOBalance
		creditAccount = AccountTransfersIn

	case SourceRoundTransfer:
		// In-round receive from another participant: same
		// counterparty treatment as OOR.
		debitAccount = AccountVTXOBalance
		creditAccount = AccountTransfersIn

	case SourceRoundBoarding:
		// Boarding or refresh of the client's own funds: the
		// offsetting leg moves on-chain wallet balance into
		// vtxo balance.
		debitAccount = AccountVTXOBalance
		creditAccount = AccountWalletBalance

	default:
		return fmt.Errorf(
			"unknown vtxo source: %q", msg.Source,
		)
	}

	return a.cfg.LedgerStore.InsertLedgerEntry(
		ctx, LedgerEntry{
			DebitAccount:  debitAccount,
			CreditAccount: creditAccount,
			AmountSat:     msg.AmountSat,
			RoundID:       roundID,
			EventType:     EventVTXOReceived,
			Description: fmt.Sprintf(
				"VTXO received via %s: %x:%d",
				msg.Source, msg.OutpointHash,
				msg.OutpointIndex,
			),
			CreatedAt: timeNowUnix(),
		},
	)
}

// handleVTXOSent records a VTXO leaving the client's balance,
// either as an out-of-round transfer (SessionID non-zero) or as
// an in-round participant-to-participant send (RoundID
// non-zero). Exactly one of the two identifiers must be set:
// both-zero is ambiguous ("unknown send context") and both-set
// is contradictory ("cannot route to both"). The counterparty
// side is debited to transfers_out so gross send flows are
// tracked independently of received flows.
func (a *LedgerActor) handleVTXOSent(
	ctx context.Context, msg *VTXOSentMsg) error {

	sessionID := sessionIDOrNil(msg.SessionID)
	roundID := roundIDOrNil(msg.RoundID)

	switch {
	case sessionID == nil && roundID == nil:
		return fmt.Errorf(
			"VTXOSentMsg requires one of SessionID " +
				"or RoundID to be non-zero",
		)

	case sessionID != nil && roundID != nil:
		return fmt.Errorf(
			"VTXOSentMsg cannot set both SessionID " +
				"and RoundID",
		)
	}

	a.log.InfoS(ctx, "Recording VTXO sent",
		slog.String("session_id",
			fmt.Sprintf("%x", msg.SessionID)),
		slog.String("round_id",
			fmt.Sprintf("%x", msg.RoundID)),
		slog.Int64("amount_sat", msg.AmountSat),
	)

	var description string
	if sessionID != nil {
		description = fmt.Sprintf(
			"VTXO sent in OOR session %x", msg.SessionID,
		)
	} else {
		description = fmt.Sprintf(
			"VTXO sent in round %x", msg.RoundID,
		)
	}

	return a.cfg.LedgerStore.InsertLedgerEntry(
		ctx, LedgerEntry{
			DebitAccount:  AccountTransfersOut,
			CreditAccount: AccountVTXOBalance,
			AmountSat:     msg.AmountSat,
			SessionID:     sessionID,
			RoundID:       roundID,
			EventType:     EventVTXOSent,
			Description:   description,
			CreatedAt:     timeNowUnix(),
		},
	)
}

// handleExitCost records a unilateral exit as two ledger entries
// that together reduce vtxo_balance by the gross exited amount:
//
//  1. Send leg: debit transfers_out += (AmountSat - ExitCostSat)
//     crediting vtxo_balance. The counterparty side captures
//     the value that actually leaves the VTXO layer.
//  2. Fee leg:  debit onchain_fees  += ExitCostSat crediting
//     vtxo_balance. The L1 miner fee portion.
//
// Both entries land in the durable actor's delivery transaction
// so either both succeed or neither does; a handler-level error
// triggers a nack and the transaction rolls back.
//
// On-chain wallet side is intentionally not booked here: the
// wallet_utxo_log audit trail covers wallet_balance changes.
func (a *LedgerActor) handleExitCost(
	ctx context.Context, msg *ExitCostMsg) error {

	a.log.InfoS(ctx, "Recording exit cost",
		slog.String("outpoint",
			fmt.Sprintf("%x:%d",
				msg.OutpointHash,
				msg.OutpointIndex)),
		slog.Int64("amount_sat", msg.AmountSat),
		slog.Int64("exit_cost_sat", msg.ExitCostSat),
		slog.Uint64("block_height",
			uint64(msg.BlockHeight)),
	)

	// Guard against pathological exits where the fee consumes
	// the entire VTXO (or more). Such an exit has no send leg
	// to record and the amount_sat > 0 CHECK would reject it
	// anyway; fail fast with a clear error instead.
	if msg.ExitCostSat <= 0 || msg.AmountSat <= 0 {
		return fmt.Errorf(
			"exit cost requires positive amount_sat and "+
				"exit_cost_sat (got %d, %d)",
			msg.AmountSat, msg.ExitCostSat,
		)
	}

	if msg.ExitCostSat >= msg.AmountSat {
		return fmt.Errorf(
			"exit cost %d exceeds or equals VTXO amount "+
				"%d for %x:%d",
			msg.ExitCostSat, msg.AmountSat,
			msg.OutpointHash, msg.OutpointIndex,
		)
	}

	now := timeNowUnix()
	netAmount := msg.AmountSat - msg.ExitCostSat

	sendLeg := LedgerEntry{
		DebitAccount:  AccountTransfersOut,
		CreditAccount: AccountVTXOBalance,
		AmountSat:     netAmount,
		EventType:     EventVTXOSent,
		Description: fmt.Sprintf(
			"unilateral exit net value for %x:%d at "+
				"height %d",
			msg.OutpointHash, msg.OutpointIndex,
			msg.BlockHeight,
		),
		CreatedAt: now,
	}

	if err := a.cfg.LedgerStore.InsertLedgerEntry(
		ctx, sendLeg,
	); err != nil {
		return fmt.Errorf("exit send leg: %w", err)
	}

	feeLeg := LedgerEntry{
		DebitAccount:  AccountOnchainFees,
		CreditAccount: AccountVTXOBalance,
		AmountSat:     msg.ExitCostSat,
		EventType:     EventOnchainFeePaid,
		Description: fmt.Sprintf(
			"exit cost for %x:%d at height %d",
			msg.OutpointHash,
			msg.OutpointIndex,
			msg.BlockHeight,
		),
		CreatedAt: now,
	}

	if err := a.cfg.LedgerStore.InsertLedgerEntry(
		ctx, feeLeg,
	); err != nil {
		return fmt.Errorf("exit fee leg: %w", err)
	}

	return nil
}

// handleUTXOCreated records a new wallet UTXO in the audit log.
// The classification is provided by the sending subsystem (e.g.
// wallet actor classifies as "deposit", round actor as "change").
func (a *LedgerActor) handleUTXOCreated(
	ctx context.Context, msg *UTXOCreatedMsg) error {

	a.log.InfoS(ctx, "Recording UTXO created",
		slog.String("outpoint",
			fmt.Sprintf("%x:%d",
				msg.OutpointHash,
				msg.OutpointIndex)),
		slog.Int64("amount_sat", msg.AmountSat),
		slog.Uint64("block_height",
			uint64(msg.BlockHeight)),
		slog.String("classification",
			msg.Classification),
	)

	if a.cfg.UTXOAuditStore == nil {
		return nil
	}

	return a.cfg.UTXOAuditStore.InsertUTXOAuditEntry(
		ctx, UTXOAuditEntry{
			OutpointHash:  msg.OutpointHash[:],
			OutpointIndex: int32(msg.OutpointIndex),
			AmountSat:     msg.AmountSat,
			Event:         "created",
			BlockHeight:   int32(msg.BlockHeight),
			ClassifiedAs:  msg.Classification,
			CreatedAt:     timeNowUnix(),
		},
	)
}

// handleUTXOSpent records a spent wallet UTXO in the audit log.
// The classification is provided by the sending subsystem (e.g.
// round actor classifies as "round_funding").
func (a *LedgerActor) handleUTXOSpent(
	ctx context.Context, msg *UTXOSpentMsg) error {

	a.log.InfoS(ctx, "Recording UTXO spent",
		slog.String("outpoint",
			fmt.Sprintf("%x:%d",
				msg.OutpointHash,
				msg.OutpointIndex)),
		slog.Int64("amount_sat", msg.AmountSat),
		slog.Uint64("block_height",
			uint64(msg.BlockHeight)),
		slog.String("classification",
			msg.Classification),
	)

	if a.cfg.UTXOAuditStore == nil {
		return nil
	}

	return a.cfg.UTXOAuditStore.InsertUTXOAuditEntry(
		ctx, UTXOAuditEntry{
			OutpointHash:  msg.OutpointHash[:],
			OutpointIndex: int32(msg.OutpointIndex),
			AmountSat:     msg.AmountSat,
			Event:         "spent",
			BlockHeight:   int32(msg.BlockHeight),
			ClassifiedAs:  msg.Classification,
			CreatedAt:     timeNowUnix(),
		},
	)
}
