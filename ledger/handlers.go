package ledger

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	fn "github.com/lightningnetwork/lnd/fn/v2"
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
//
// Validation and entry construction run before Commit, with no writer
// lock held; only the single InsertLedgerEntry runs inside the
// lease-fenced Commit transaction.
func (a *LedgerActor) handleFeePaid(ctx context.Context, msg *FeePaidMsg,
	ax actor.Exec[ledgerTx]) fn.Result[LedgerResp] {

	const errMsg = "Failed to handle fee paid"

	// Reject non-positive amounts up front so a malformed TLV
	// (e.g. a zero payload or a uint64 that decoded past
	// math.MaxInt64) surfaces as ErrInvalidMessage instead of
	// hitting the SQL CHECK constraint and driving a durable
	// retry loop on a permanent failure.
	if msg.AmountSat <= 0 {
		return a.fail(
			ctx, errMsg, fmt.Errorf("%w: FeePaidMsg amount_sat "+
				"must be positive (got %d)", ErrInvalidMessage,
				msg.AmountSat),
		)
	}

	roundID := roundIDOrNil(msg.RoundID)

	// Operator-fee types share the same accounts (fees_paid /
	// vtxo_balance). Onchain-sweep fees book against onchain_fees /
	// wallet_clearing instead — they are L1 chain costs paid by a
	// wallet-internal sweep, not Ark protocol operator fees, and the
	// fee is settled through wallet clearing rather than VTXO balance.
	var (
		eventType     string
		debitAccount  string
		creditAccount string
		idempotency   []byte
		description   string
	)
	switch msg.FeeType {
	case FeeTypeBoarding:
		eventType = EventBoardingFeePaid
		debitAccount = AccountFeesPaid
		creditAccount = AccountVTXOBalance
		description = fmt.Sprintf("%s fee paid in round %x",
			msg.FeeType, msg.RoundID)

	case FeeTypeRefresh:
		eventType = EventRefreshFeePaid
		debitAccount = AccountFeesPaid
		creditAccount = AccountVTXOBalance
		description = fmt.Sprintf("%s fee paid in round %x",
			msg.FeeType, msg.RoundID)

	case FeeTypeOnchainSweep:
		eventType = EventBoardingSweepFeePaid
		debitAccount = AccountOnchainFees
		creditAccount = AccountWalletClearing

		// Onchain-sweep fees are not associated with a round.
		// Use the sweep txid (carried in IdempotencyKey by the
		// caller) to dedup replays via the
		// idx_client_ledger_idempotent_key partial unique index.
		// Round-keyed dedup is intentionally bypassed by setting
		// roundID to nil below.
		if len(msg.IdempotencyKey) != chainhash.HashSize {
			return a.fail(
				ctx, errMsg,
				fmt.Errorf(
					"%w: FeePaidMsg onchain-sweep "+
						"idempotency_key must be %d "+
						"bytes (got %d)",
					ErrInvalidMessage, chainhash.HashSize,
					len(msg.IdempotencyKey),
				),
			)
		}

		roundID = nil
		idempotency = msg.IdempotencyKey
		description = "boarding-sweep on-chain cost"

	default:
		return a.fail(
			ctx, errMsg, fmt.Errorf("%w: unknown fee type %q",
				ErrInvalidMessage, msg.FeeType),
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

	entry := LedgerEntry{
		DebitAccount:   debitAccount,
		CreditAccount:  creditAccount,
		AmountSat:      msg.AmountSat,
		RoundID:        roundID,
		IdempotencyKey: idempotency,
		EventType:      eventType,
		Description:    description,
		CreatedAt:      a.clk.Now().Unix(),
	}

	return a.commit(ctx, ax, errMsg, func(ctx context.Context,
		q ledgerTx) error {

		return q.ledger.InsertLedgerEntry(ctx, entry)
	})
}

// handleVTXOReceived records a VTXO received by the client.
// For OOR transfers, the counterparty side is booked to
// transfers_in (debit vtxo_balance, credit transfers_in). For
// round receipts, the balance moves from wallet_balance to
// vtxo_balance.
func (a *LedgerActor) handleVTXOReceived(ctx context.Context,
	msg *VTXOReceivedMsg, ax actor.Exec[ledgerTx]) fn.Result[LedgerResp] {

	const errMsg = "Failed to handle VTXO received"

	// Reject non-positive amounts up front; see handleFeePaid
	// for the rationale.
	if msg.AmountSat <= 0 {
		return a.fail(
			ctx, errMsg, fmt.Errorf("%w: VTXOReceivedMsg "+
				"amount_sat must be positive (got %d)",
				ErrInvalidMessage, msg.AmountSat),
		)
	}

	roundID := roundIDOrNil(msg.RoundID)

	a.log.InfoS(ctx, "Recording VTXO received",
		slog.String(
			"outpoint", fmt.Sprintf("%x:%d", msg.OutpointHash,
				msg.OutpointIndex),
		),
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
		return a.fail(
			ctx, errMsg, fmt.Errorf("%w: unknown vtxo source %q",
				ErrInvalidMessage, msg.Source),
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

	// Surface the VTXO outpoint on the row's structured chain
	// fields too. Without these the consumer-facing onchain view
	// renders a "round"-kind entry with an empty txid and has to
	// string-parse the description to recover the outpoint — see
	// issue #504.
	chainVout := int32(msg.OutpointIndex)

	entry := LedgerEntry{
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
		ChainTxid:      msg.OutpointHash[:],
		ChainVout:      &chainVout,
	}

	return a.commit(ctx, ax, errMsg, func(ctx context.Context,
		q ledgerTx) error {

		return q.ledger.InsertLedgerEntry(ctx, entry)
	})
}

// handleVTXOSent records a VTXO leaving the client's balance,
// either as an out-of-round transfer (SessionID non-zero) or as
// an in-round participant-to-participant send (RoundID
// non-zero). Exactly one of the two identifiers must be set:
// both-zero is ambiguous ("unknown send context") and both-set
// is contradictory ("cannot route to both"). The counterparty
// side is debited to transfers_out so gross send flows are
// tracked independently of received flows.
func (a *LedgerActor) handleVTXOSent(ctx context.Context, msg *VTXOSentMsg,
	ax actor.Exec[ledgerTx]) fn.Result[LedgerResp] {

	const errMsg = "Failed to handle VTXO sent"

	// Reject non-positive amounts up front; see handleFeePaid
	// for the rationale.
	if msg.AmountSat <= 0 {
		return a.fail(
			ctx, errMsg, fmt.Errorf("%w: VTXOSentMsg amount_sat "+
				"must be positive (got %d)", ErrInvalidMessage,
				msg.AmountSat),
		)
	}

	sessionID := sessionIDOrNil(msg.SessionID)
	roundID := roundIDOrNil(msg.RoundID)

	switch {
	case sessionID == nil && roundID == nil:
		return a.fail(
			ctx, errMsg, fmt.Errorf("%w: VTXOSentMsg requires "+
				"one of SessionID or RoundID to be non-zero",
				ErrInvalidMessage),
		)

	case sessionID != nil && roundID != nil:
		return a.fail(
			ctx, errMsg, fmt.Errorf("%w: VTXOSentMsg cannot set "+
				"both SessionID and RoundID",
				ErrInvalidMessage),
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
		description = fmt.Sprintf("VTXO sent in OOR session %x",
			msg.SessionID)
	} else {
		description = fmt.Sprintf("VTXO sent in round %x", msg.RoundID)
	}

	// A caller-supplied key is used for round-scoped sends that do
	// not correspond to a local VTXO outpoint, such as cooperative
	// leave outputs and foreign directed-send recipient outputs.
	// Otherwise an outpoint-derived key disambiguates per-VTXO sends.
	var idempotencyKey []byte
	switch {
	case len(msg.IdempotencyKey) > 0:
		idempotencyKey = msg.IdempotencyKey

	case !msg.Outpoint.Hash.IsEqual(&zeroHash):
		idempotencyKey = walletUTXOIdempotencyKey(
			msg.Outpoint.Hash, msg.Outpoint.Index,
		)
	}

	entry := LedgerEntry{
		DebitAccount:   AccountTransfersOut,
		CreditAccount:  AccountVTXOBalance,
		AmountSat:      msg.AmountSat,
		SessionID:      sessionID,
		RoundID:        roundID,
		EventType:      EventVTXOSent,
		Description:    description,
		CreatedAt:      a.clk.Now().Unix(),
		IdempotencyKey: idempotencyKey,
	}

	return a.commit(ctx, ax, errMsg, func(ctx context.Context,
		q ledgerTx) error {

		return q.ledger.InsertLedgerEntry(ctx, entry)
	})
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
// On-chain wallet-side balance movement is intentionally not booked
// here: this handler records the VTXO-funded exit value and confirmed
// sweep cost supplied by the unroll path.
func (a *LedgerActor) handleExitCost(ctx context.Context, msg *ExitCostMsg,
	ax actor.Exec[ledgerTx]) fn.Result[LedgerResp] {

	const errMsg = "Failed to handle exit cost"

	a.log.InfoS(ctx, "Recording exit cost",
		slog.String(
			"outpoint", fmt.Sprintf("%x:%d", msg.OutpointHash,
				msg.OutpointIndex),
		),
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
		return a.fail(
			ctx, errMsg, fmt.Errorf("%w: exit cost requires "+
				"positive amount_sat and exit_cost_sat "+
				"(got %d, %d)", ErrInvalidMessage,
				msg.AmountSat, msg.ExitCostSat),
		)
	}

	if msg.ExitCostSat >= msg.AmountSat {
		return a.fail(
			ctx, errMsg, fmt.Errorf("%w: exit cost %d exceeds or "+
				"equals VTXO amount %d for %x:%d",
				ErrInvalidMessage, msg.ExitCostSat,
				msg.AmountSat, msg.OutpointHash,
				msg.OutpointIndex),
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

	// Book the send leg and the fee leg via two InsertLedgerEntry
	// calls inside ONE Commit. Both join the same lease-fenced writer
	// transaction, so a crash or error between them rolls back both
	// writes and the mailbox ack together -- no partial-write window.
	// The shared outpoint-derived IdempotencyKey makes an out-of-band
	// replay resolve to a silent no-op via the partial unique index
	// idx_client_ledger_idempotent_key and the ON CONFLICT DO NOTHING
	// clause on the insert query.
	return a.commit(ctx, ax, errMsg, func(ctx context.Context,
		q ledgerTx) error {

		if err := q.ledger.InsertLedgerEntry(ctx, sendLeg); err != nil {
			return fmt.Errorf("exit send leg: %w", err)
		}

		if err := q.ledger.InsertLedgerEntry(ctx, feeLeg); err != nil {
			return fmt.Errorf("exit fee leg: %w", err)
		}

		return nil
	})
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
func (a *LedgerActor) handleUTXOCreated(ctx context.Context,
	msg *UTXOCreatedMsg, ax actor.Exec[ledgerTx]) fn.Result[LedgerResp] {

	const errMsg = "Failed to handle UTXO created"

	a.log.InfoS(ctx, "Recording UTXO created",
		slog.String(
			"outpoint", fmt.Sprintf("%x:%d", msg.OutpointHash,
				msg.OutpointIndex),
		),
		slog.Int64("amount_sat", msg.AmountSat),
		slog.Uint64("block_height",
			uint64(msg.BlockHeight)),
		slog.String("classification",
			msg.Classification),
	)

	// UTXOAuditStore is optional: in log-only mode there is nothing to
	// persist, so consume the message without opening a Commit.
	if a.cfg.UTXOAuditStore == nil {
		return fn.Ok[LedgerResp](nil)
	}

	if msg.AmountSat <= 0 {
		return a.fail(
			ctx, errMsg, fmt.Errorf("%w: UTXOCreatedMsg "+
				"amount_sat must be positive (got %d)",
				ErrInvalidMessage, msg.AmountSat),
		)
	}

	now := a.clk.Now().Unix()
	chainVout := int32(msg.OutpointIndex)
	confirmationHeight := int32(msg.BlockHeight)

	audit := UTXOAuditEntry{
		OutpointHash:  msg.OutpointHash[:],
		OutpointIndex: int32(msg.OutpointIndex),
		AmountSat:     msg.AmountSat,
		Event:         "created",
		BlockHeight:   int32(msg.BlockHeight),
		ClassifiedAs:  msg.Classification,
		CreatedAt:     now,
	}

	creditAccount := AccountOpeningBalance
	if msg.Classification == ClassificationBoardingSweepReturn {
		creditAccount = AccountWalletClearing
	}

	entry := LedgerEntry{
		DebitAccount:  AccountWalletBalance,
		CreditAccount: creditAccount,
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
		ChainTxid:          msg.OutpointHash[:],
		ChainVout:          &chainVout,
		ConfirmationHeight: &confirmationHeight,
	}

	// The audit row and the ledger deposit leg commit together in one
	// lease-fenced transaction.
	return a.commit(ctx, ax, errMsg, func(ctx context.Context,
		q ledgerTx) error {

		if err := q.audit.InsertUTXOAuditEntry(ctx, audit); err != nil {
			return err
		}

		return q.ledger.InsertLedgerEntry(ctx, entry)
	})
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
func (a *LedgerActor) handleUTXOSpent(ctx context.Context, msg *UTXOSpentMsg,
	ax actor.Exec[ledgerTx]) fn.Result[LedgerResp] {

	const errMsg = "Failed to handle UTXO spent"

	a.log.InfoS(ctx, "Recording UTXO spent",
		slog.String(
			"outpoint", fmt.Sprintf("%x:%d", msg.OutpointHash,
				msg.OutpointIndex),
		),
		slog.Int64("amount_sat", msg.AmountSat),
		slog.Uint64("block_height",
			uint64(msg.BlockHeight)),
		slog.String("classification",
			msg.Classification),
	)

	// UTXOAuditStore is optional: in log-only mode there is nothing to
	// persist, so consume the message without opening a Commit.
	if a.cfg.UTXOAuditStore == nil {
		return fn.Ok[LedgerResp](nil)
	}

	now := a.clk.Now().Unix()
	audit := UTXOAuditEntry{
		OutpointHash:  msg.OutpointHash[:],
		OutpointIndex: int32(msg.OutpointIndex),
		AmountSat:     msg.AmountSat,
		Event:         "spent",
		BlockHeight:   int32(msg.BlockHeight),
		ClassifiedAs:  msg.Classification,
		CreatedAt:     now,
	}

	// Only the boarding-sweep-input classification books a double-entry
	// ledger leg (debit wallet_clearing, credit wallet_balance). All other
	// classifications are audit-only, and wallet_utxo_log has no positivity
	// CHECK, so the non-positive guard is scoped to the ledger-emitting
	// branch. Rejecting unconditionally would turn an audit-only spend with
	// a zero amount into a durable-mailbox poison-pill.
	var entry *LedgerEntry
	if msg.Classification == ClassificationBoardingSweepInput {
		if msg.AmountSat <= 0 {
			return a.fail(
				ctx, errMsg, fmt.Errorf("%w: UTXOSpentMsg "+
					"amount_sat must be positive (got %d)",
					ErrInvalidMessage, msg.AmountSat),
			)
		}

		chainVout := int32(msg.OutpointIndex)
		confirmationHeight := int32(msg.BlockHeight)
		entry = &LedgerEntry{
			DebitAccount:  AccountWalletClearing,
			CreditAccount: AccountWalletBalance,
			AmountSat:     msg.AmountSat,
			EventType:     EventWalletUTXOSpent,
			Description: fmt.Sprintf(
				"wallet UTXO spent at %x:%d "+
					"(classification %s) at height %d",
				msg.OutpointHash, msg.OutpointIndex,
				msg.Classification, msg.BlockHeight,
			),
			CreatedAt: now,
			IdempotencyKey: walletUTXOIdempotencyKey(
				msg.OutpointHash, msg.OutpointIndex,
			),
			ChainTxid:          msg.OutpointHash[:],
			ChainVout:          &chainVout,
			ConfirmationHeight: &confirmationHeight,
		}
	}

	return a.commit(ctx, ax, errMsg, func(ctx context.Context,
		q ledgerTx) error {

		if err := q.audit.InsertUTXOAuditEntry(ctx, audit); err != nil {
			return err
		}

		if entry == nil {
			return nil
		}

		return q.ledger.InsertLedgerEntry(ctx, *entry)
	})
}

// handleBoardingSweepConfirmed books every leg of a confirmed boarding
// sweep inside a single Commit so the wallet_clearing account is updated
// atomically: the fee leg, one audit + clearing-debit leg per spent input,
// and the destination leg (an external transfer out, or a wallet-return
// deposit). Either the whole clearing set lands or none of it does, so a
// partial failure can never strand value in wallet_clearing.
//
// The per-leg idempotency keys match the historical single-message keys
// (sweep txid for the fee, outpoint for each input, txid vout 0 for a
// wallet return, "wallet-sweep:"+txid for an external transfer) so a replay
// — including an in-flight message straddling an upgrade — dedups cleanly.
func (a *LedgerActor) handleBoardingSweepConfirmed(ctx context.Context,
	msg *BoardingSweepConfirmedMsg,
	ax actor.Exec[ledgerTx]) fn.Result[LedgerResp] {

	const errMsg = "Failed to handle boarding sweep confirmed"

	// Validate the aggregate and per-input amounts up front so a
	// malformed message dead-letters cleanly instead of hitting a SQL
	// CHECK partway through the commit.
	if msg.ChainCostSat <= 0 {
		return a.fail(
			ctx, errMsg, fmt.Errorf("%w: "+
				"BoardingSweepConfirmedMsg chain_cost_sat "+
				"must be positive (got %d)", ErrInvalidMessage,
				msg.ChainCostSat),
		)
	}
	if msg.DestinationSat <= 0 {
		return a.fail(
			ctx, errMsg, fmt.Errorf("%w: "+
				"BoardingSweepConfirmedMsg destination_sat "+
				"must be positive (got %d)", ErrInvalidMessage,
				msg.DestinationSat),
		)
	}
	if len(msg.Inputs) == 0 {
		return a.fail(
			ctx, errMsg, fmt.Errorf("%w: "+
				"BoardingSweepConfirmedMsg must carry at "+
				"least one input", ErrInvalidMessage),
		)
	}
	for _, in := range msg.Inputs {
		if in.AmountSat <= 0 {
			return a.fail(
				ctx, errMsg, fmt.Errorf("%w: "+
					"BoardingSweepConfirmedMsg input %s "+
					"amount_sat must be positive (got %d)",
					ErrInvalidMessage, in.Outpoint,
					in.AmountSat),
			)
		}
	}

	a.log.InfoS(ctx, "Recording boarding sweep confirmed",
		slog.String("txid", fmt.Sprintf("%x", msg.Txid)),
		slog.Int64("chain_cost_sat", msg.ChainCostSat),
		slog.Int("num_inputs", len(msg.Inputs)),
		slog.Int64("destination_sat", msg.DestinationSat),
		slog.Bool("destination_external", msg.DestinationExternal),
		slog.Uint64("block_height", uint64(msg.BlockHeight)),
	)

	// Build every leg up front so the commit closure stays a thin,
	// shallow insert sequence and the whole set is booked atomically.
	legs := a.boardingSweepLegs(msg)

	return a.commit(ctx, ax, errMsg, func(ctx context.Context,
		q ledgerTx) error {

		for _, leg := range legs {
			if leg.hasAudit && q.audit != nil {
				err := q.audit.InsertUTXOAuditEntry(
					ctx, leg.audit,
				)
				if err != nil {
					return err
				}
			}

			if err := q.ledger.InsertLedgerEntry(
				ctx, leg.entry,
			); err != nil {
				return err
			}
		}

		return nil
	})
}

// sweepLeg pairs an optional UTXO audit row with the double-entry ledger row
// that a single boarding-sweep clearing leg writes. hasAudit is false for
// the fee and external-transfer legs, which touch only the ledger.
type sweepLeg struct {
	hasAudit bool
	audit    UTXOAuditEntry
	entry    LedgerEntry
}

// boardingSweepLegs builds the ordered set of ledger (and audit) legs a
// confirmed boarding sweep books: the fee leg, one audit + clearing-debit
// leg per spent input, and the destination leg (an external transfer out,
// or a wallet-return deposit with its audit row). Building the set outside
// the commit keeps the transaction body shallow and lets the handler insert
// every leg in one Commit.
func (a *LedgerActor) boardingSweepLegs(
	msg *BoardingSweepConfirmedMsg) []sweepLeg {

	now := a.clk.Now().Unix()
	confirmationHeight := int32(msg.BlockHeight)

	legs := make([]sweepLeg, 0, len(msg.Inputs)+2)

	// Fee leg: debit onchain_fees, credit wallet_clearing.
	legs = append(legs, sweepLeg{
		entry: LedgerEntry{
			DebitAccount:   AccountOnchainFees,
			CreditAccount:  AccountWalletClearing,
			AmountSat:      msg.ChainCostSat,
			EventType:      EventBoardingSweepFeePaid,
			Description:    "boarding-sweep on-chain cost",
			CreatedAt:      now,
			IdempotencyKey: append([]byte(nil), msg.Txid[:]...),
		},
	})

	// Per-input audit row + wallet_clearing debit leg.
	for _, in := range msg.Inputs {
		hash := [32]byte(in.Outpoint.Hash)
		chainVout := int32(in.Outpoint.Index)

		audit := UTXOAuditEntry{
			OutpointHash:  in.Outpoint.Hash[:],
			OutpointIndex: int32(in.Outpoint.Index),
			AmountSat:     in.AmountSat,
			Event:         "spent",
			BlockHeight:   int32(msg.BlockHeight),
			ClassifiedAs:  ClassificationBoardingSweepInput,
			CreatedAt:     now,
		}

		entry := LedgerEntry{
			DebitAccount:  AccountWalletClearing,
			CreditAccount: AccountWalletBalance,
			AmountSat:     in.AmountSat,
			EventType:     EventWalletUTXOSpent,
			Description: fmt.Sprintf(
				"wallet UTXO spent at %x:%d "+
					"(classification %s) at height %d",
				in.Outpoint.Hash, in.Outpoint.Index,
				ClassificationBoardingSweepInput,
				msg.BlockHeight,
			),
			CreatedAt: now,
			IdempotencyKey: walletUTXOIdempotencyKey(
				hash, in.Outpoint.Index,
			),
			ChainTxid:          in.Outpoint.Hash[:],
			ChainVout:          &chainVout,
			ConfirmationHeight: &confirmationHeight,
		}

		legs = append(legs, sweepLeg{
			hasAudit: true,
			audit:    audit,
			entry:    entry,
		})
	}

	// External destination: the funds left the wallet, so settle the
	// value to transfers_out and close the clearing account.
	if msg.DestinationExternal {
		legs = append(legs, sweepLeg{
			entry: LedgerEntry{
				DebitAccount:  AccountTransfersOut,
				CreditAccount: AccountWalletClearing,
				AmountSat:     msg.DestinationSat,
				EventType:     EventWalletSweepTransfer,
				Description: fmt.Sprintf(
					"wallet sweep external transfer for "+
						"txid %x at height %d",
					msg.Txid, msg.BlockHeight,
				),
				CreatedAt: now,
				IdempotencyKey: append(
					[]byte("wallet-sweep:"), msg.Txid[:]...,
				),
				ChainTxid:          msg.Txid[:],
				ConfirmationHeight: &confirmationHeight,
			},
		})

		return legs
	}

	// Wallet-return destination: the swept value re-enters the wallet as
	// a new UTXO at vout 0. Record the audit "created" row and the
	// wallet_balance deposit that closes clearing.
	returnVout := int32(0)
	returnAudit := UTXOAuditEntry{
		OutpointHash:  msg.Txid[:],
		OutpointIndex: 0,
		AmountSat:     msg.DestinationSat,
		Event:         "created",
		BlockHeight:   int32(msg.BlockHeight),
		ClassifiedAs:  ClassificationBoardingSweepReturn,
		CreatedAt:     now,
	}

	returnEntry := LedgerEntry{
		DebitAccount:  AccountWalletBalance,
		CreditAccount: AccountWalletClearing,
		AmountSat:     msg.DestinationSat,
		EventType:     EventWalletUTXOCreated,
		Description: fmt.Sprintf(
			"wallet UTXO confirmed at %x:%d (classification %s) "+
				"at height %d",
			msg.Txid, 0, ClassificationBoardingSweepReturn,
			msg.BlockHeight,
		),
		CreatedAt: now,
		IdempotencyKey: walletUTXOIdempotencyKey(
			msg.Txid, 0,
		),
		ChainTxid:          msg.Txid[:],
		ChainVout:          &returnVout,
		ConfirmationHeight: &confirmationHeight,
	}

	legs = append(legs, sweepLeg{
		hasAudit: true,
		audit:    returnAudit,
		entry:    returnEntry,
	})

	return legs
}
