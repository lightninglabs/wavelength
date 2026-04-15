package ledger

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// zeroRoundID is the zero value used to detect empty round IDs.
var zeroRoundID [16]byte

// roundIDOrNil converts a 16-byte round ID to a slice, returning
// nil for zero-valued IDs so the database stores NULL (which
// correctly bypasses the conditional idempotency unique index).
func roundIDOrNil(id [16]byte) []byte {
	if id == zeroRoundID {
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
	case "boarding":
		eventType = EventBoardingFeePaid
	case "refresh":
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
	case "oor":
		// OOR receive: income from a transfer.
		debitAccount = AccountVTXOBalance
		creditAccount = AccountTransfersIn

	case "round":
		// Round receive (boarding/refresh): on-chain
		// wallet funds converted to VTXO balance.
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

// handleVTXOSent records VTXOs sent by the client during an OOR
// transfer. The VTXO balance decreases (credit) and the
// transfer is recorded as an outflow.
func (a *LedgerActor) handleVTXOSent(
	ctx context.Context, msg *VTXOSentMsg) error {

	a.log.InfoS(ctx, "Recording VTXO sent",
		slog.String("session_id",
			fmt.Sprintf("%x", msg.SessionID)),
		slog.Int64("amount_sat", msg.AmountSat),
	)

	return a.cfg.LedgerStore.InsertLedgerEntry(
		ctx, LedgerEntry{
			DebitAccount:  AccountTransfersIn,
			CreditAccount: AccountVTXOBalance,
			AmountSat:     msg.AmountSat,
			RoundID:       msg.SessionID[:],
			EventType:     EventVTXOSent,
			Description: fmt.Sprintf(
				"VTXO sent in OOR session %x",
				msg.SessionID,
			),
			CreatedAt: timeNowUnix(),
		},
	)
}

// handleExitCost records the on-chain fee cost when the client
// performs a unilateral exit. The exit cost is an expense debited
// from onchain_fees and credited against vtxo_balance.
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

	return a.cfg.LedgerStore.InsertLedgerEntry(
		ctx, LedgerEntry{
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
			CreatedAt: timeNowUnix(),
		},
	)
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
