package fees

import (
	"context"
	"time"
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

	// LedgerEventOORTransfer is recorded for OOR transfer
	// volume tracking (no fee charged).
	LedgerEventOORTransfer LedgerEventType = "oor_transfer"
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
const (
	AccountTreasuryWallet  AccountID = "treasury_wallet"
	AccountDeployedCapital AccountID = "deployed_capital"
	AccountUserVTXOClaims  AccountID = "user_vtxo_claims"
	AccountOperatorRevenue AccountID = "operator_revenue"
	AccountMiningFees      AccountID = "mining_fees"
)

// AllAccounts returns the full chart of accounts. Useful for
// tests that assert the seed data and the Go constants stay in
// sync.
func AllAccounts() []AccountID {
	return []AccountID{
		AccountTreasuryWallet,
		AccountDeployedCapital,
		AccountUserVTXOClaims,
		AccountOperatorRevenue,
		AccountMiningFees,
	}
}

// LedgerEntry is the domain-level representation of a
// double-entry ledger record. This decouples the fees package
// from the sqlc-generated types.
type LedgerEntry struct {
	DebitAccount  AccountID
	CreditAccount AccountID
	AmountSat     int64
	RoundID       []byte
	EventType     LedgerEventType
	Description   string
	CreatedAt     int64
}

// LedgerStore is the interface for persisting ledger entries.
// Implementations bridge to the sqlc-generated queries via the
// db package.
type LedgerStore interface {
	InsertLedgerEntry(
		ctx context.Context, entry LedgerEntry,
	) error
}

// RecordBoardingDeposit records the user's deposit entering the
// shared output (net of fee). The deposit increases deployed
// capital and creates a corresponding user VTXO claim. Debits
// deployed_capital, credits user_vtxo_claims.
func RecordBoardingDeposit(
	ctx context.Context, store LedgerStore,
	roundID []byte, amountSat int64,
	now time.Time) error {

	return store.InsertLedgerEntry(ctx, LedgerEntry{
		DebitAccount:  AccountDeployedCapital,
		CreditAccount: AccountUserVTXOClaims,
		AmountSat:     amountSat,
		RoundID:       roundID,
		EventType:     LedgerEventBoardingDeposit,
		Description:   "boarding deposit into shared output",
		CreatedAt:     now.Unix(),
	})
}

// RecordBoardingFee records fee collection from a boarding
// input. The fee is carved out of the deposit that lands in
// the shared output before the user's VTXO claim is created,
// so it debits deployed_capital and credits operator_revenue.
func RecordBoardingFee(
	ctx context.Context, store LedgerStore,
	roundID []byte, amountSat int64,
	now time.Time) error {

	return store.InsertLedgerEntry(ctx, LedgerEntry{
		DebitAccount:  AccountDeployedCapital,
		CreditAccount: AccountOperatorRevenue,
		AmountSat:     amountSat,
		RoundID:       roundID,
		EventType:     LedgerEventBoardingFee,
		Description:   "boarding input fee",
		CreatedAt:     now.Unix(),
	})
}

// RecordRefreshFee records fee collection from a
// forfeit/refresh. The fee reduces the user's outstanding
// claim, so it debits user_vtxo_claims and credits
// operator_revenue.
func RecordRefreshFee(
	ctx context.Context, store LedgerStore,
	roundID []byte, amountSat int64,
	now time.Time) error {

	return store.InsertLedgerEntry(ctx, LedgerEntry{
		DebitAccount:  AccountUserVTXOClaims,
		CreditAccount: AccountOperatorRevenue,
		AmountSat:     amountSat,
		RoundID:       roundID,
		EventType:     LedgerEventRefreshFee,
		Description:   "refresh/forfeit fee",
		CreatedAt:     now.Unix(),
	})
}

// RecordRefreshForfeit records the retirement of the user's old
// VTXO claim when they forfeit it as part of a refresh. The old
// claim is returned to deployed capital. Debits
// user_vtxo_claims, credits deployed_capital.
func RecordRefreshForfeit(
	ctx context.Context, store LedgerStore,
	roundID []byte, amountSat int64,
	now time.Time) error {

	return store.InsertLedgerEntry(ctx, LedgerEntry{
		DebitAccount:  AccountUserVTXOClaims,
		CreditAccount: AccountDeployedCapital,
		AmountSat:     amountSat,
		RoundID:       roundID,
		EventType:     LedgerEventRefreshForfeit,
		Description:   "forfeit old VTXO claim",
		CreatedAt:     now.Unix(),
	})
}

// RecordRefreshNewVTXO records the issuance of a new VTXO claim
// after a refresh. The operator deploys capital to back the new
// VTXO. Debits deployed_capital, credits user_vtxo_claims.
func RecordRefreshNewVTXO(
	ctx context.Context, store LedgerStore,
	roundID []byte, amountSat int64,
	now time.Time) error {

	return store.InsertLedgerEntry(ctx, LedgerEntry{
		DebitAccount:  AccountDeployedCapital,
		CreditAccount: AccountUserVTXOClaims,
		AmountSat:     amountSat,
		RoundID:       roundID,
		EventType:     LedgerEventRefreshNewVTXO,
		Description:   "new VTXO claim issued after refresh",
		CreatedAt:     now.Unix(),
	})
}

// RecordOffboard records a user exiting Ark by offboarding
// their VTXO to an on-chain output. The user's claim is
// retired and the payout comes from the operator's treasury.
// Debits user_vtxo_claims, credits treasury_wallet.
func RecordOffboard(
	ctx context.Context, store LedgerStore,
	roundID []byte, amountSat int64,
	now time.Time) error {

	return store.InsertLedgerEntry(ctx, LedgerEntry{
		DebitAccount:  AccountUserVTXOClaims,
		CreditAccount: AccountTreasuryWallet,
		AmountSat:     amountSat,
		RoundID:       roundID,
		EventType:     LedgerEventOffboard,
		Description:   "offboard VTXO to on-chain output",
		CreatedAt:     now.Unix(),
	})
}

// RecordMiningFee records on-chain mining fees paid for a round
// transaction. Debits mining_fees, credits treasury_wallet.
func RecordMiningFee(
	ctx context.Context, store LedgerStore,
	roundID []byte, amountSat int64,
	now time.Time) error {

	return store.InsertLedgerEntry(ctx, LedgerEntry{
		DebitAccount:  AccountMiningFees,
		CreditAccount: AccountTreasuryWallet,
		AmountSat:     amountSat,
		RoundID:       roundID,
		EventType:     LedgerEventMiningFee,
		Description:   "round transaction mining fee",
		CreatedAt:     now.Unix(),
	})
}

// RecordCapitalCommitted records capital committed to fund new
// VTXOs in a round. Debits deployed_capital, credits
// treasury_wallet.
func RecordCapitalCommitted(
	ctx context.Context, store LedgerStore,
	roundID []byte, amountSat int64,
	now time.Time) error {

	return store.InsertLedgerEntry(ctx, LedgerEntry{
		DebitAccount:  AccountDeployedCapital,
		CreditAccount: AccountTreasuryWallet,
		AmountSat:     amountSat,
		RoundID:       roundID,
		EventType:     LedgerEventCapitalCommitted,
		Description:   "capital committed to round VTXOs",
		CreatedAt:     now.Unix(),
	})
}

// RecordRoundSweep records capital returned from sweeping
// expired VTXOs. Debits treasury_wallet, credits
// deployed_capital.
func RecordRoundSweep(
	ctx context.Context, store LedgerStore,
	roundID []byte, amountSat int64,
	now time.Time) error {

	return store.InsertLedgerEntry(ctx, LedgerEntry{
		DebitAccount:  AccountTreasuryWallet,
		CreditAccount: AccountDeployedCapital,
		AmountSat:     amountSat,
		RoundID:       roundID,
		EventType:     LedgerEventRoundSweep,
		Description:   "expired VTXO sweep reclaimed",
		CreatedAt:     now.Unix(),
	})
}
