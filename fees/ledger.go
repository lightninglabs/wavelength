package fees

import (
	"context"
	"time"

	"github.com/btcsuite/btcd/btcutil"
)

// LedgerEventType classifies ledger entries for filtering and
// reporting.
type LedgerEventType string

const (
	// LedgerEventBoardingFee is recorded when a boarding input
	// pays a fee to the operator.
	LedgerEventBoardingFee LedgerEventType = "boarding_fee"

	// LedgerEventRefreshFee is recorded when a forfeit/refresh
	// pays a fee to the operator.
	LedgerEventRefreshFee LedgerEventType = "refresh_fee"

	// LedgerEventMiningFee is recorded when the operator pays
	// on-chain mining fees for a round transaction.
	LedgerEventMiningFee LedgerEventType = "mining_fee"

	// LedgerEventCapitalCommitted is recorded when operator
	// capital is committed to fund new VTXOs in a round.
	LedgerEventCapitalCommitted LedgerEventType = "capital_committed"

	// LedgerEventRoundSweep is recorded when the operator
	// sweeps expired VTXOs back into the wallet, reclaiming
	// previously deployed capital.
	LedgerEventRoundSweep LedgerEventType = "round_sweep"

	// LedgerEventBoardingDeposit is recorded when a user's
	// on-chain deposit enters the shared output, creating a
	// new VTXO claim (net of fee).
	LedgerEventBoardingDeposit LedgerEventType = "boarding_deposit"

	// LedgerEventRefreshForfeit is recorded when the user
	// forfeits their old VTXO as part of a refresh, retiring
	// the old claim.
	LedgerEventRefreshForfeit LedgerEventType = "refresh_forfeit"

	// LedgerEventRefreshNewVTXO is recorded when a new VTXO
	// is issued to the user after a refresh, creating a fresh
	// claim.
	LedgerEventRefreshNewVTXO LedgerEventType = "refresh_new_vtxo"

	// LedgerEventOffboard is recorded when a user exits Ark
	// by offboarding their VTXO back to an on-chain output.
	LedgerEventOffboard LedgerEventType = "offboard"

	// LedgerEventOffboardFee is recorded when an offboard
	// operation pays a fee to the operator. The fee comes from
	// the user's outstanding claim.
	LedgerEventOffboardFee LedgerEventType = "offboard_fee"

	// LedgerEventOORTransfer is recorded for OOR transfer fees.
	// Today OOR transfers are free (zero fee), so the helper
	// that would emit this event is gated on fee > 0.
	LedgerEventOORTransfer LedgerEventType = "oor_transfer"

	// LedgerEventExternalDeposit is recorded when the wallet
	// UTXO diff subsystem detects a UTXO arriving in the
	// treasury wallet that does not originate from a round
	// sweep or round change. This represents operator capital
	// contribution into the business.
	LedgerEventExternalDeposit LedgerEventType = "external_deposit"

	// LedgerEventExternalWithdrawal is recorded when the wallet
	// UTXO diff subsystem observes the operator spending from
	// the treasury wallet to an address that is not a round
	// transaction. This represents operator capital extraction.
	LedgerEventExternalWithdrawal LedgerEventType = "external_withdrawal"
)

// AccountID is the typed identifier for a chart-of-accounts entry.
// The string form is written directly to the `accounts.account_id`
// column and must match a seeded row in the accounting migration.
// The typed wrapper catches typos and account/event confusion at
// compile time (mirroring the LedgerEventType pattern).
type AccountID string

// String returns the account_id as a raw string, useful for
// direct interpolation into sqlc parameters.
func (a AccountID) String() string {
	return string(a)
}

// Account identifiers matching the seeded accounts table. Keep
// these in lockstep with db/sqlc/migrations/000010_accounting.up.sql.
// Fee revenue is split per product (boarding, refresh, offboard,
// OOR) so tax reporting and business analytics can read gross
// per-product numbers directly from account balances.
const (
	AccountTreasuryWallet     AccountID = "treasury_wallet"
	AccountDeployedCapital    AccountID = "deployed_capital"
	AccountUserVTXOClaims     AccountID = "user_vtxo_claims"
	AccountBoardingFeeRevenue AccountID = "boarding_fee_revenue"
	AccountRefreshFeeRevenue  AccountID = "refresh_fee_revenue"
	AccountOffboardFeeRevenue AccountID = "offboard_fee_revenue"
	AccountOORFeeRevenue      AccountID = "oor_fee_revenue"
	AccountMiningFees         AccountID = "mining_fees"
	AccountExternalFunding    AccountID = "external_funding"
)

// AllAccounts returns the full chart of accounts. Useful for
// tests that assert the seed data and the Go constants stay in
// sync.
func AllAccounts() []AccountID {
	return []AccountID{
		AccountTreasuryWallet,
		AccountDeployedCapital,
		AccountUserVTXOClaims,
		AccountBoardingFeeRevenue,
		AccountRefreshFeeRevenue,
		AccountOffboardFeeRevenue,
		AccountOORFeeRevenue,
		AccountMiningFees,
		AccountExternalFunding,
	}
}

// LedgerEntry is the domain-level representation of a
// double-entry ledger record. This decouples the fees package
// from the sqlc-generated types.
//
// Exactly one of RoundID or SessionID should be set for events
// scoped to a round or an OOR session. Events that belong to
// neither (external deposits/withdrawals from the UTXO diff
// subsystem) leave both nil — the `CHECK (round_id IS NULL OR
// session_id IS NULL)` schema constraint enforces the mutual
// exclusion at the database layer.
type LedgerEntry struct {
	// DebitAccount is the chart-of-accounts entry whose
	// balance increases (for assets/expenses) or decreases
	// (for liabilities/revenue/equity) by Amount.
	DebitAccount AccountID

	// CreditAccount is the chart-of-accounts entry whose
	// balance moves in the opposite direction from
	// DebitAccount by Amount. Must differ from DebitAccount;
	// the schema's `CHECK (debit_account <> credit_account)`
	// rejects self-transfers.
	CreditAccount AccountID

	// Amount is the satoshi value moved between DebitAccount
	// and CreditAccount. Must be strictly positive — the
	// schema's `CHECK (amount_sat > 0)` rejects zero-value
	// entries.
	Amount btcutil.Amount

	// RoundID is the optional 16-byte round identifier this
	// entry attaches to. Set for round-scoped events
	// (boarding, refresh, offboard, mining, capital_committed,
	// round_sweep). Mutually exclusive with SessionID.
	RoundID []byte

	// SessionID is the optional 32-byte OOR session identifier
	// this entry attaches to. Set for OOR-scoped events.
	// Mutually exclusive with RoundID.
	SessionID []byte

	// IdempotencyKey is an opaque caller-supplied identifier
	// used by the partial unique index
	// (idempotency_key, event_type, debit_account,
	// credit_account) to make at-least-once mailbox replay a
	// silent no-op. Leaving it nil opts out of dedup; tests
	// and one-shot admin writes may do so, but durable-actor
	// call sites should always set it.
	IdempotencyKey []byte

	// EventType classifies this entry per the seeded
	// ledger_event_types enum. Must match a row in that
	// table (schema enforces the FK).
	EventType LedgerEventType

	// Description is a free-form human-readable annotation.
	// Event classification belongs in EventType; Description
	// is for operator notes only.
	Description string

	// CreatedAt is the wall-clock time the entry was
	// recorded. The DB adapter flattens this to a Unix
	// timestamp when writing; the domain type preserves
	// time.Time so callers do not truncate precision
	// prematurely and can format/compare timestamps directly.
	CreatedAt time.Time
}

// LedgerStore is the interface for persisting ledger entries.
// Implementations bridge to the sqlc-generated queries via the
// db package.
type LedgerStore interface {
	InsertLedgerEntry(
		ctx context.Context, entry LedgerEntry,
	) error
}

// roundIdempotencyKey derives the idempotency key for a
// round-scoped event. Using the raw round_id is sufficient
// because the partial unique index also discriminates on
// event_type, debit_account, and credit_account — so different
// events in the same round carry distinct index tuples even
// when their idempotency keys are identical.
func roundIdempotencyKey(roundID []byte) []byte {
	if len(roundID) == 0 {
		return nil
	}
	key := make([]byte, len(roundID))
	copy(key, roundID)

	return key
}

// sessionIdempotencyKey is the OOR analogue of
// roundIdempotencyKey.
func sessionIdempotencyKey(sessionID []byte) []byte {
	if len(sessionID) == 0 {
		return nil
	}
	key := make([]byte, len(sessionID))
	copy(key, sessionID)

	return key
}

// RecordBoardingDeposit records the user's deposit entering the
// shared output (net of fee). The deposit increases deployed
// capital and creates a corresponding user VTXO claim. Debits
// deployed_capital, credits user_vtxo_claims.
func RecordBoardingDeposit(
	ctx context.Context, store LedgerStore,
	roundID []byte, amountSat btcutil.Amount,
	now time.Time) error {

	return store.InsertLedgerEntry(ctx, LedgerEntry{
		DebitAccount:   AccountDeployedCapital,
		CreditAccount:  AccountUserVTXOClaims,
		Amount:         amountSat,
		RoundID:        roundID,
		IdempotencyKey: roundIdempotencyKey(roundID),
		EventType:      LedgerEventBoardingDeposit,
		Description:    "boarding deposit into shared output",
		CreatedAt:      now,
	})
}

// RecordBoardingFee records fee collection from a boarding
// input. The fee is carved from the deposit before the user's
// VTXO claim is created, so it debits deployed_capital and
// credits boarding_fee_revenue.
func RecordBoardingFee(
	ctx context.Context, store LedgerStore,
	roundID []byte, amountSat btcutil.Amount,
	now time.Time) error {

	return store.InsertLedgerEntry(ctx, LedgerEntry{
		DebitAccount:   AccountDeployedCapital,
		CreditAccount:  AccountBoardingFeeRevenue,
		Amount:         amountSat,
		RoundID:        roundID,
		IdempotencyKey: roundIdempotencyKey(roundID),
		EventType:      LedgerEventBoardingFee,
		Description:    "boarding input fee",
		CreatedAt:      now,
	})
}

// RecordRefreshFee records fee collection from a
// forfeit/refresh. The fee reduces the user's outstanding
// claim, so it debits user_vtxo_claims and credits
// refresh_fee_revenue.
func RecordRefreshFee(
	ctx context.Context, store LedgerStore,
	roundID []byte, amountSat btcutil.Amount,
	now time.Time) error {

	return store.InsertLedgerEntry(ctx, LedgerEntry{
		DebitAccount:   AccountUserVTXOClaims,
		CreditAccount:  AccountRefreshFeeRevenue,
		Amount:         amountSat,
		RoundID:        roundID,
		IdempotencyKey: roundIdempotencyKey(roundID),
		EventType:      LedgerEventRefreshFee,
		Description:    "refresh/forfeit fee",
		CreatedAt:      now,
	})
}

// RecordRefreshForfeit records the retirement of the user's old
// VTXO claim when they forfeit it as part of a refresh. The old
// claim is returned to deployed capital. Debits
// user_vtxo_claims, credits deployed_capital.
func RecordRefreshForfeit(
	ctx context.Context, store LedgerStore,
	roundID []byte, amountSat btcutil.Amount,
	now time.Time) error {

	return store.InsertLedgerEntry(ctx, LedgerEntry{
		DebitAccount:   AccountUserVTXOClaims,
		CreditAccount:  AccountDeployedCapital,
		Amount:         amountSat,
		RoundID:        roundID,
		IdempotencyKey: roundIdempotencyKey(roundID),
		EventType:      LedgerEventRefreshForfeit,
		Description:    "forfeit old VTXO claim",
		CreatedAt:      now,
	})
}

// RecordRefreshNewVTXO records the issuance of a new VTXO claim
// after a refresh. The operator deploys capital to back the new
// VTXO. Debits deployed_capital, credits user_vtxo_claims.
func RecordRefreshNewVTXO(
	ctx context.Context, store LedgerStore,
	roundID []byte, amountSat btcutil.Amount,
	now time.Time) error {

	return store.InsertLedgerEntry(ctx, LedgerEntry{
		DebitAccount:   AccountDeployedCapital,
		CreditAccount:  AccountUserVTXOClaims,
		Amount:         amountSat,
		RoundID:        roundID,
		IdempotencyKey: roundIdempotencyKey(roundID),
		EventType:      LedgerEventRefreshNewVTXO,
		Description:    "new VTXO claim issued after refresh",
		CreatedAt:      now,
	})
}

// RecordOffboard records a user exiting Ark by offboarding
// their VTXO to an on-chain output. The user's claim is
// retired and the payout comes from the operator's treasury.
// Debits user_vtxo_claims, credits treasury_wallet.
func RecordOffboard(
	ctx context.Context, store LedgerStore,
	roundID []byte, amountSat btcutil.Amount,
	now time.Time) error {

	return store.InsertLedgerEntry(ctx, LedgerEntry{
		DebitAccount:   AccountUserVTXOClaims,
		CreditAccount:  AccountTreasuryWallet,
		Amount:         amountSat,
		RoundID:        roundID,
		IdempotencyKey: roundIdempotencyKey(roundID),
		EventType:      LedgerEventOffboard,
		Description:    "offboard VTXO to on-chain output",
		CreatedAt:      now,
	})
}

// RecordOffboardFee records fee collection from an offboard.
// The fee reduces the user's outstanding claim before the
// payout lands on-chain, so it debits user_vtxo_claims and
// credits offboard_fee_revenue.
func RecordOffboardFee(
	ctx context.Context, store LedgerStore,
	roundID []byte, amountSat btcutil.Amount,
	now time.Time) error {

	return store.InsertLedgerEntry(ctx, LedgerEntry{
		DebitAccount:   AccountUserVTXOClaims,
		CreditAccount:  AccountOffboardFeeRevenue,
		Amount:         amountSat,
		RoundID:        roundID,
		IdempotencyKey: roundIdempotencyKey(roundID),
		EventType:      LedgerEventOffboardFee,
		Description:    "offboard fee",
		CreatedAt:      now,
	})
}

// RecordMiningFee records on-chain mining fees paid for a round
// transaction. Debits mining_fees, credits treasury_wallet.
func RecordMiningFee(
	ctx context.Context, store LedgerStore,
	roundID []byte, amountSat btcutil.Amount,
	now time.Time) error {

	return store.InsertLedgerEntry(ctx, LedgerEntry{
		DebitAccount:   AccountMiningFees,
		CreditAccount:  AccountTreasuryWallet,
		Amount:         amountSat,
		RoundID:        roundID,
		IdempotencyKey: roundIdempotencyKey(roundID),
		EventType:      LedgerEventMiningFee,
		Description:    "round transaction mining fee",
		CreatedAt:      now,
	})
}

// RecordCapitalCommitted records capital committed to fund new
// VTXOs in a round. Debits deployed_capital, credits
// treasury_wallet.
func RecordCapitalCommitted(
	ctx context.Context, store LedgerStore,
	roundID []byte, amountSat btcutil.Amount,
	now time.Time) error {

	return store.InsertLedgerEntry(ctx, LedgerEntry{
		DebitAccount:   AccountDeployedCapital,
		CreditAccount:  AccountTreasuryWallet,
		Amount:         amountSat,
		RoundID:        roundID,
		IdempotencyKey: roundIdempotencyKey(roundID),
		EventType:      LedgerEventCapitalCommitted,
		Description:    "capital committed to round VTXOs",
		CreatedAt:      now,
	})
}

// RecordOORTransfer records an OOR transfer fee. Today OOR
// transfers are free and this helper is not invoked (the
// handler gates on fee > 0), but the plumbing is in place so
// that when OOR fees are introduced the event lands in the
// ledger alongside other fee events. OOR fees come from the
// user's outstanding claim, so the debit/credit pair mirrors
// RecordRefreshFee: debits user_vtxo_claims, credits
// oor_fee_revenue. The 32-byte session identifier is passed
// through the SessionID column; the schema's round/session
// mutual-exclusion CHECK keeps RoundID null for OOR entries.
func RecordOORTransfer(
	ctx context.Context, store LedgerStore,
	sessionID []byte, feeSat btcutil.Amount,
	now time.Time) error {

	return store.InsertLedgerEntry(ctx, LedgerEntry{
		DebitAccount:   AccountUserVTXOClaims,
		CreditAccount:  AccountOORFeeRevenue,
		Amount:         feeSat,
		SessionID:      sessionID,
		IdempotencyKey: sessionIdempotencyKey(sessionID),
		EventType:      LedgerEventOORTransfer,
		Description:    "OOR transfer fee",
		CreatedAt:      now,
	})
}

// RecordRoundSweep records capital returned from sweeping
// expired VTXOs. Debits treasury_wallet, credits
// deployed_capital.
func RecordRoundSweep(
	ctx context.Context, store LedgerStore,
	roundID []byte, amountSat btcutil.Amount,
	now time.Time) error {

	return store.InsertLedgerEntry(ctx, LedgerEntry{
		DebitAccount:   AccountTreasuryWallet,
		CreditAccount:  AccountDeployedCapital,
		Amount:         amountSat,
		RoundID:        roundID,
		IdempotencyKey: roundIdempotencyKey(roundID),
		EventType:      LedgerEventRoundSweep,
		Description:    "expired VTXO sweep reclaimed",
		CreatedAt:      now,
	})
}

// RecordExternalDeposit records an operator capital contribution
// arriving in the treasury wallet from outside the Ark system
// (e.g. on-chain funding from the business's operational
// accounts). Debits treasury_wallet, credits external_funding
// (equity). The idempotency key must uniquely identify the
// specific UTXO that surfaced the deposit — typically the
// 36-byte concatenation of the outpoint hash and index — so
// that the wallet UTXO diff loop can be replayed without
// re-booking the same deposit.
func RecordExternalDeposit(
	ctx context.Context, store LedgerStore,
	idempotencyKey []byte, amountSat btcutil.Amount,
	now time.Time) error {

	return store.InsertLedgerEntry(ctx, LedgerEntry{
		DebitAccount:   AccountTreasuryWallet,
		CreditAccount:  AccountExternalFunding,
		Amount:         amountSat,
		IdempotencyKey: idempotencyKey,
		EventType:      LedgerEventExternalDeposit,
		Description:    "external deposit into treasury wallet",
		CreatedAt:      now,
	})
}

// RecordExternalWithdrawal records the operator extracting
// capital from the treasury wallet to a destination outside
// the Ark system. Debits external_funding, credits
// treasury_wallet — the inverse of RecordExternalDeposit.
func RecordExternalWithdrawal(
	ctx context.Context, store LedgerStore,
	idempotencyKey []byte, amountSat btcutil.Amount,
	now time.Time) error {

	return store.InsertLedgerEntry(ctx, LedgerEntry{
		DebitAccount:   AccountExternalFunding,
		CreditAccount:  AccountTreasuryWallet,
		Amount:         amountSat,
		IdempotencyKey: idempotencyKey,
		EventType:      LedgerEventExternalWithdrawal,
		Description:    "external withdrawal from treasury wallet",
		CreatedAt:      now,
	})
}
