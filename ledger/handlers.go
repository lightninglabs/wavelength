package ledger

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
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

// handleFeePaid records a fee payment by the client. Fees paid
// during boarding or refresh are debited from fees_paid and
// credited to vtxo_balance (the fee reduces the client's VTXO
// balance).
func (a *LedgerActor) handleFeePaid(
	ctx context.Context, msg *FeePaidMsg) error {

	// Reject non-positive amounts up front so a malformed TLV
	// (e.g. a zero payload or a uint64 that decoded past
	// math.MaxInt64) surfaces as ErrInvalidMessage instead of
	// hitting the SQL CHECK constraint and driving a durable
	// retry loop on a permanent failure.
	if msg.AmountSat <= 0 {
		return fmt.Errorf(
			"%w: FeePaidMsg amount_sat must be positive "+
				"(got %d)",
			ErrInvalidMessage, msg.AmountSat,
		)
	}

	roundID := roundIDOrNil(msg.RoundID)

	// Map the fee type string to the appropriate event type.
	var eventType string
	switch msg.FeeType {
	case FeeTypeBoarding:
		eventType = EventBoardingFeePaid
	case FeeTypeRefresh:
		eventType = EventRefreshFeePaid
	default:
		return fmt.Errorf(
			"%w: unknown fee type %q",
			ErrInvalidMessage, msg.FeeType,
		)
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
			CreatedAt: a.clk.Now().Unix(),
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

	// Reject non-positive amounts up front; see handleFeePaid
	// for the rationale.
	if msg.AmountSat <= 0 {
		return fmt.Errorf(
			"%w: VTXOReceivedMsg amount_sat must be "+
				"positive (got %d)",
			ErrInvalidMessage, msg.AmountSat,
		)
	}

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
		// Boarding of the client's own on-chain funds: the
		// offsetting leg moves wallet_balance value into
		// vtxo_balance. Refresh is NOT booked here; refresh
		// uses SourceRoundRefresh so wallet_balance doesn't
		// drift on a flow that never touched the wallet.
		debitAccount = AccountVTXOBalance
		creditAccount = AccountWalletBalance

	case SourceRoundRefresh:
		// Refresh output (including directed-send self-change):
		// the VTXO came from a forfeited VTXO in the same round,
		// not from the wallet. Credit transfers_out so this leg
		// cancels the companion VTXOSentMsg's transfers_out
		// debit for the gross forfeited amount. Net effect on
		// transfers_out is zero; net effect on vtxo_balance is
		// exactly the operator fee, which the paired
		// FeePaidMsg(refresh) removes.
		debitAccount = AccountVTXOBalance
		creditAccount = AccountTransfersOut

	default:
		return fmt.Errorf(
			"%w: unknown vtxo source %q",
			ErrInvalidMessage, msg.Source,
		)
	}

	// Per-VTXO idempotency key so multiple owned receives in
	// the same round (three-way directed send, multi-leg refresh,
	// a round with both a boarding intent and a received transfer)
	// don't collide on idx_client_ledger_idempotent_round. The
	// partial round/session indexes stay as defense-in-depth
	// against a caller that omits the outpoint.
	idempotencyKey := walletUTXOIdempotencyKey(
		msg.OutpointHash, msg.OutpointIndex,
	)

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
			CreatedAt:      a.clk.Now().Unix(),
			IdempotencyKey: idempotencyKey,
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

	// Reject non-positive amounts up front; see handleFeePaid
	// for the rationale.
	if msg.AmountSat <= 0 {
		return fmt.Errorf(
			"%w: VTXOSentMsg amount_sat must be positive "+
				"(got %d)",
			ErrInvalidMessage, msg.AmountSat,
		)
	}

	sessionID := sessionIDOrNil(msg.SessionID)
	roundID := roundIDOrNil(msg.RoundID)

	switch {
	case sessionID == nil && roundID == nil:
		return fmt.Errorf(
			"%w: VTXOSentMsg requires one of SessionID "+
				"or RoundID to be non-zero",
			ErrInvalidMessage,
		)

	case sessionID != nil && roundID != nil:
		return fmt.Errorf(
			"%w: VTXOSentMsg cannot set both SessionID "+
				"and RoundID",
			ErrInvalidMessage,
		)
	}

	a.log.InfoS(ctx, "Recording VTXO sent",
		slog.String("session_id",
			fmt.Sprintf("%x", msg.SessionID)),
		slog.String("round_id",
			fmt.Sprintf("%x", msg.RoundID)),
		slog.String("outpoint", msg.Outpoint.String()),
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

	// When an outpoint is supplied, stamp an outpoint-derived
	// idempotency key so two sends in the same round (e.g. two
	// refreshes) do not collide on
	// idx_client_ledger_idempotent_round. Messages without an
	// outpoint fall back to that round/session partial index
	// as before.
	var idempotencyKey []byte
	if !msg.Outpoint.Hash.IsEqual(&zeroHash) {
		idempotencyKey = walletUTXOIdempotencyKey(
			msg.Outpoint.Hash, msg.Outpoint.Index,
		)
	}

	return a.cfg.LedgerStore.InsertLedgerEntry(
		ctx, LedgerEntry{
			DebitAccount:   AccountTransfersOut,
			CreditAccount:  AccountVTXOBalance,
			AmountSat:      msg.AmountSat,
			SessionID:      sessionID,
			RoundID:        roundID,
			EventType:      EventVTXOSent,
			Description:    description,
			CreatedAt:      a.clk.Now().Unix(),
			IdempotencyKey: idempotencyKey,
		},
	)
}

// zeroHash is a convenience sentinel for detecting an absent
// wire.OutPoint hash.
var zeroHash chainhash.Hash

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
// via two InsertLedgerEntry calls that join the outer tx. Either
// both commit or neither does: a handler-level error returns
// non-nil, the durable actor nacks, and the whole tx (including
// a possibly-successful first insert) rolls back. Redelivery of
// a committed message cannot happen because Ack/MarkProcessed
// land in the same tx; defensive protection against out-of-band
// replays is provided by the shared outpoint-derived
// IdempotencyKey on both legs, which hits the partial unique
// index idx_client_ledger_idempotent_key and is swallowed by the
// adapter's ON CONFLICT DO NOTHING.
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
			"%w: exit cost requires positive amount_sat "+
				"and exit_cost_sat (got %d, %d)",
			ErrInvalidMessage, msg.AmountSat,
			msg.ExitCostSat,
		)
	}

	if msg.ExitCostSat >= msg.AmountSat {
		return fmt.Errorf(
			"%w: exit cost %d exceeds or equals VTXO "+
				"amount %d for %x:%d",
			ErrInvalidMessage, msg.ExitCostSat,
			msg.AmountSat, msg.OutpointHash,
			msg.OutpointIndex,
		)
	}

	now := a.clk.Now().Unix()
	netAmount := msg.AmountSat - msg.ExitCostSat
	idempotencyKey := exitIdempotencyKey(
		msg.OutpointHash, msg.OutpointIndex,
	)

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
		CreatedAt:      now,
		IdempotencyKey: idempotencyKey,
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
		CreatedAt:      now,
		IdempotencyKey: idempotencyKey,
	}

	// Book the send leg and the fee leg via two separate
	// InsertLedgerEntry calls. Both join the durable actor's
	// outer delivery transaction (db.TransactionExecutor.ExecTx
	// picks up the tx from ctx via actor.TxFromContext), so a
	// crash or error between the two calls rolls back both
	// writes and the mailbox ack together -- no partial-write
	// window. The shared outpoint-derived IdempotencyKey makes
	// an out-of-band replay resolve to a silent no-op via the
	// partial unique index idx_client_ledger_idempotent_key and
	// the ON CONFLICT DO NOTHING clause on the insert query.
	if err := a.cfg.LedgerStore.InsertLedgerEntry(
		ctx, sendLeg,
	); err != nil {
		return fmt.Errorf("exit send leg: %w", err)
	}

	if err := a.cfg.LedgerStore.InsertLedgerEntry(
		ctx, feeLeg,
	); err != nil {
		return fmt.Errorf("exit fee leg: %w", err)
	}

	return nil
}

// exitIdempotencyKey derives the outpoint-scoped dedup key used on
// ExitCost ledger entries. Packing hash (32 bytes) and index (4
// bytes) into a single BLOB lets both exit legs share a key and
// share the idempotency index, while staying distinct across
// outpoints that only differ in the index (same tx, different
// output).
func exitIdempotencyKey(hash [32]byte, index uint32) []byte {
	out := make([]byte, 32+4)
	copy(out[:32], hash[:])
	out[32] = byte(index >> 24)
	out[33] = byte(index >> 16)
	out[34] = byte(index >> 8)
	out[35] = byte(index)

	return out
}

// handleUTXOCreated records a new wallet UTXO in two places:
//
//  1. The wallet_utxo_log audit trail via UTXOAuditStore, tagged
//     with the caller-supplied classification.
//  2. The double-entry ledger as a deposit leg "debit
//     wallet_balance, credit opening_balance". opening_balance
//     is an equity account acting as the source of funds. This
//     leg is what balances the matching "debit vtxo_balance,
//     credit wallet_balance" leg that SourceRoundBoarding writes
//     when the same wallet UTXO is later consumed by a round;
//     without this deposit leg wallet_balance would drift negative
//     on every boarding.
//
// Both inserts join the outer durable-actor transaction via
// actor.TxFromContext / db.TransactionExecutor.ExecTx, so a crash
// between them rolls back both together with the mailbox ack.
// The ledger leg uses an outpoint-derived idempotency key so a
// replayed UTXOCreatedMsg dedupes silently via the partial unique
// index idx_client_ledger_idempotent_key.
//
// UTXOAuditStore is optional: when nil, both the audit entry and
// the ledger entry are skipped (the actor is in "log-only" mode).
// This mirrors the pre-existing behavior; callers wanting the
// double-entry row must wire the audit store.
//
// Non-positive amounts are rejected up front with ErrInvalidMessage
// so a malformed TLV dead-letters instead of hitting the SQL
// CHECK (amount_sat > 0) and driving an infinite nack-and-retry
// loop. A zero/negative on-chain UTXO is impossible in practice
// (wire enforces MaxSatoshi bounds on tx outputs) but the guard
// closes the last corruption gap on the TLV decode path.
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

	if msg.AmountSat <= 0 {
		return fmt.Errorf(
			"%w: UTXOCreatedMsg amount_sat must be positive "+
				"(got %d)",
			ErrInvalidMessage, msg.AmountSat,
		)
	}

	now := a.clk.Now().Unix()

	err := a.cfg.UTXOAuditStore.InsertUTXOAuditEntry(
		ctx, UTXOAuditEntry{
			OutpointHash:  msg.OutpointHash[:],
			OutpointIndex: int32(msg.OutpointIndex),
			AmountSat:     msg.AmountSat,
			Event:         "created",
			BlockHeight:   int32(msg.BlockHeight),
			ClassifiedAs:  msg.Classification,
			CreatedAt:     now,
		},
	)
	if err != nil {
		return err
	}

	return a.cfg.LedgerStore.InsertLedgerEntry(
		ctx, LedgerEntry{
			DebitAccount:  AccountWalletBalance,
			CreditAccount: AccountOpeningBalance,
			AmountSat:     msg.AmountSat,
			EventType:     EventWalletUTXOCreated,
			Description: fmt.Sprintf(
				"wallet UTXO confirmed at %x:%d "+
					"(classification %s) at height %d",
				msg.OutpointHash, msg.OutpointIndex,
				msg.Classification, msg.BlockHeight,
			),
			CreatedAt: now,
			IdempotencyKey: walletUTXOIdempotencyKey(
				msg.OutpointHash, msg.OutpointIndex,
			),
		},
	)
}

// walletUTXOIdempotencyKey derives the outpoint-scoped dedup key
// used on the wallet UTXO deposit ledger leg. Same encoding as
// exitIdempotencyKey but kept as a distinct helper so a future
// change to one scheme (e.g. collision domain split) doesn't
// silently affect the other.
func walletUTXOIdempotencyKey(hash [32]byte, index uint32) []byte {
	out := make([]byte, 32+4)
	copy(out[:32], hash[:])
	out[32] = byte(index >> 24)
	out[33] = byte(index >> 16)
	out[34] = byte(index >> 8)
	out[35] = byte(index)

	return out
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
			CreatedAt:     a.clk.Now().Unix(),
		},
	)
}
