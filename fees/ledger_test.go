package fees

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// captureStore is a LedgerStore that records each insert into a
// callback for assertion. It keeps the Record* helper tests
// decoupled from the sql layer.
type captureStore struct {
	on func(entry LedgerEntry)
}

// InsertLedgerEntry forwards the entry to the test callback.
func (c captureStore) InsertLedgerEntry(
	_ context.Context, entry LedgerEntry) error {

	c.on(entry)

	return nil
}

// fixedTime returns a deterministic timestamp for use in ledger
// recording tests.
func fixedTime() time.Time {
	return time.Date(
		2026, 1, 1, 0, 0, 0, 0, time.UTC,
	)
}

// TestAllAccounts ensures every typed AccountID constant shows up
// in AllAccounts. This is the Go-side half of the chart-of-
// accounts cross-check; the DB-side test that compares this
// set against the seeded migration lives in
// db.TestLedgerStoreDBAccountsMatchChartOfAccounts.
func TestAllAccounts(t *testing.T) {
	t.Parallel()

	got := AllAccounts()

	require.ElementsMatch(t, []AccountID{
		AccountTreasuryWallet,
		AccountDeployedCapital,
		AccountUserVTXOClaims,
		AccountBoardingFeeRevenue,
		AccountRefreshFeeRevenue,
		AccountOffboardFeeRevenue,
		AccountOORFeeRevenue,
		AccountMiningFees,
		AccountExternalFunding,
	}, got)

	// No duplicates.
	seen := make(map[AccountID]struct{}, len(got))
	for _, a := range got {
		_, dup := seen[a]
		require.False(
			t, dup,
			"duplicate AccountID in AllAccounts: %v", a,
		)
		seen[a] = struct{}{}
	}
}

// TestAccountIDString verifies the String() round-trip so that
// AccountID remains a drop-in replacement for the raw string
// previously stored in the chart of accounts column.
func TestAccountIDString(t *testing.T) {
	t.Parallel()

	require.Equal(t, "treasury_wallet",
		AccountTreasuryWallet.String())
	require.Equal(t, "deployed_capital",
		AccountDeployedCapital.String())
	require.Equal(t, "user_vtxo_claims",
		AccountUserVTXOClaims.String())
	require.Equal(t, "boarding_fee_revenue",
		AccountBoardingFeeRevenue.String())
	require.Equal(t, "refresh_fee_revenue",
		AccountRefreshFeeRevenue.String())
	require.Equal(t, "offboard_fee_revenue",
		AccountOffboardFeeRevenue.String())
	require.Equal(t, "oor_fee_revenue",
		AccountOORFeeRevenue.String())
	require.Equal(t, "mining_fees", AccountMiningFees.String())
	require.Equal(t, "external_funding",
		AccountExternalFunding.String())
}

// TestRecordHelpersUseSeededAccounts pins down each Record*
// helper's (debit, credit) pair so an accidental rename or swap
// would fail loudly. The expected pairs are copied directly from
// the "Double-Entry Accounting" table in docs/fee-model.md.
func TestRecordHelpersUseSeededAccounts(t *testing.T) {
	t.Parallel()

	type captured struct {
		entry LedgerEntry
	}

	call := func(
		fn func(store LedgerStore) error) LedgerEntry {

		c := &captured{}
		store := captureStore{on: func(e LedgerEntry) {
			c.entry = e
		}}
		require.NoError(t, fn(store))

		return c.entry
	}

	type helper struct {
		name   string
		run    func(store LedgerStore) error
		debit  AccountID
		credit AccountID
		event  LedgerEventType
	}

	now := fixedTime()
	helpers := []helper{
		{
			name: "BoardingDeposit",
			run: func(s LedgerStore) error {
				return RecordBoardingDeposit(
					t.Context(), s, nil, 1, now,
				)
			},
			debit:  AccountDeployedCapital,
			credit: AccountUserVTXOClaims,
			event:  LedgerEventBoardingDeposit,
		},
		{
			name: "BoardingFee",
			run: func(s LedgerStore) error {
				return RecordBoardingFee(
					t.Context(), s, nil, 1, now,
				)
			},
			debit:  AccountDeployedCapital,
			credit: AccountBoardingFeeRevenue,
			event:  LedgerEventBoardingFee,
		},
		{
			name: "RefreshFee",
			run: func(s LedgerStore) error {
				return RecordRefreshFee(
					t.Context(), s, nil, 1, now,
				)
			},
			debit:  AccountUserVTXOClaims,
			credit: AccountRefreshFeeRevenue,
			event:  LedgerEventRefreshFee,
		},
		{
			name: "OffboardFee",
			run: func(s LedgerStore) error {
				return RecordOffboardFee(
					t.Context(), s, nil, 1, now,
				)
			},
			debit:  AccountUserVTXOClaims,
			credit: AccountOffboardFeeRevenue,
			event:  LedgerEventOffboardFee,
		},
		{
			name: "OORTransfer",
			run: func(s LedgerStore) error {
				return RecordOORTransfer(
					t.Context(), s, nil, 1, now,
				)
			},
			debit:  AccountUserVTXOClaims,
			credit: AccountOORFeeRevenue,
			event:  LedgerEventOORTransfer,
		},
		{
			name: "ExternalDeposit",
			run: func(s LedgerStore) error {
				return RecordExternalDeposit(
					t.Context(), s, []byte("k"), 1, now,
				)
			},
			debit:  AccountTreasuryWallet,
			credit: AccountExternalFunding,
			event:  LedgerEventExternalDeposit,
		},
		{
			name: "ExternalWithdrawal",
			run: func(s LedgerStore) error {
				return RecordExternalWithdrawal(
					t.Context(), s, []byte("k"), 1, now,
				)
			},
			debit:  AccountExternalFunding,
			credit: AccountTreasuryWallet,
			event:  LedgerEventExternalWithdrawal,
		},
		{
			name: "RefreshForfeit",
			run: func(s LedgerStore) error {
				return RecordRefreshForfeit(
					t.Context(), s, nil, 1, now,
				)
			},
			debit:  AccountUserVTXOClaims,
			credit: AccountDeployedCapital,
			event:  LedgerEventRefreshForfeit,
		},
		{
			name: "RefreshNewVTXO",
			run: func(s LedgerStore) error {
				return RecordRefreshNewVTXO(
					t.Context(), s, nil, 1, now,
				)
			},
			debit:  AccountDeployedCapital,
			credit: AccountUserVTXOClaims,
			event:  LedgerEventRefreshNewVTXO,
		},
		{
			name: "Offboard",
			run: func(s LedgerStore) error {
				return RecordOffboard(
					t.Context(), s, nil, 1, now,
				)
			},
			debit:  AccountUserVTXOClaims,
			credit: AccountTreasuryWallet,
			event:  LedgerEventOffboard,
		},
		{
			name: "MiningFee",
			run: func(s LedgerStore) error {
				return RecordMiningFee(
					t.Context(), s, nil, 1, now,
				)
			},
			debit:  AccountMiningFees,
			credit: AccountTreasuryWallet,
			event:  LedgerEventMiningFee,
		},
		{
			name: "CapitalCommitted",
			run: func(s LedgerStore) error {
				return RecordCapitalCommitted(
					t.Context(), s, nil, 1, now,
				)
			},
			debit:  AccountDeployedCapital,
			credit: AccountTreasuryWallet,
			event:  LedgerEventCapitalCommitted,
		},
		{
			name: "RoundSweep",
			run: func(s LedgerStore) error {
				return RecordRoundSweep(
					t.Context(), s, nil, 1, now,
				)
			},
			debit:  AccountTreasuryWallet,
			credit: AccountDeployedCapital,
			event:  LedgerEventRoundSweep,
		},
	}

	for _, h := range helpers {
		h := h
		t.Run(h.name, func(t *testing.T) {
			t.Parallel()

			got := call(h.run)
			require.Equal(t, h.debit, got.DebitAccount)
			require.Equal(t, h.credit, got.CreditAccount)
			require.Equal(t, h.event, got.EventType)
			require.NotEqual(
				t, got.DebitAccount, got.CreditAccount,
				"double-entry requires distinct accounts",
			)
		})
	}
}
