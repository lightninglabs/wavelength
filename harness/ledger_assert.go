package harness

import (
	"context"
	"fmt"
	"testing"

	"github.com/lightninglabs/darepo/adminrpc"
	"github.com/stretchr/testify/require"
)

// Account names for the double-entry ledger. These mirror the
// seeded chart of accounts in db/sqlc/migrations/000010_accounting.
// Kept as unexported string constants so tests that build an
// ExpectedDelta cannot typo an account name and silently pass.
const (
	AccTreasuryWallet     = "treasury_wallet"
	AccDeployedCapital    = "deployed_capital"
	AccUserVTXOClaims     = "user_vtxo_claims"
	AccBoardingFeeRevenue = "boarding_fee_revenue"
	AccRefreshFeeRevenue  = "refresh_fee_revenue"
	AccOffboardFeeRevenue = "offboard_fee_revenue"
	AccOORFeeRevenue      = "oor_fee_revenue"
	AccMiningFees         = "mining_fees"
	AccExternalFunding    = "external_funding"
)

// AllAccounts enumerates every account in the seeded chart of
// accounts so callers can zero-initialize a balance map without
// risk of missing one.
func AllAccounts() []string {
	return []string{
		AccTreasuryWallet,
		AccDeployedCapital,
		AccUserVTXOClaims,
		AccBoardingFeeRevenue,
		AccRefreshFeeRevenue,
		AccOffboardFeeRevenue,
		AccOORFeeRevenue,
		AccMiningFees,
		AccExternalFunding,
	}
}

// LedgerSnapshot is a point-in-time view of the operator's
// double-entry ledger. Balances map account names to signed
// balances computed by debiting an account and crediting the
// counterparty: credits add, debits subtract.
//
// Snapshots are deliberately computed client-side from the
// ListFeeEvents admin RPC instead of adding a new
// GetAccountBalance surface. Test ledgers carry few entries so
// the O(n) walk is cheap, and the assertion stays independent
// of any future admin-RPC refactor.
type LedgerSnapshot struct {
	// Balances is the signed per-account balance. A missing
	// account name is treated as zero by Delta().
	Balances map[string]int64

	// TotalEntries is the total count of ledger entries at
	// snapshot time (equivalent to
	// ListFeeEventsResponse.Total).
	TotalEntries uint32

	// MaxEntryID is the largest entry_id observed at snapshot
	// time. Used by AssertLedgerDelta to sanity-check that the
	// after-snapshot strictly grew, catching accidental
	// ledger-entry rollback.
	MaxEntryID int64

	// EventsByType is the count of entries grouped by
	// event_type, so tests can assert "exactly one
	// refresh_fee" was booked.
	EventsByType map[string]int
}

// Delta returns a signed per-account balance delta (after minus
// before). A missing key on either side is treated as zero.
func (s LedgerSnapshot) Delta(other LedgerSnapshot) map[string]int64 {
	out := make(map[string]int64, len(AllAccounts()))
	for _, acc := range AllAccounts() {
		out[acc] = other.Balances[acc] - s.Balances[acc]
	}

	return out
}

// ExpectedDelta describes the per-account balance shift plus
// the expected count and event-type breakdown of new ledger
// entries between two snapshots. Fields left zero are asserted
// to be zero; missing keys in the maps are treated as zero.
type ExpectedDelta struct {
	// Balances is the expected signed delta per account. Any
	// account not named is asserted to have a zero delta,
	// which is a strong guarantee that no unintended
	// double-entry leg landed.
	Balances map[string]int64

	// NewEntries is the expected increase in TotalEntries.
	// Zero means "no new entries expected" (strict assertion).
	NewEntries int

	// EventsByType is the expected increase in per-event-type
	// counts. Missing keys assert a zero delta.
	EventsByType map[string]int
}

// TakeLedgerSnapshot walks the operator's ledger via the admin
// RPC ListFeeEvents (paginating until every entry is read) and
// builds a point-in-time LedgerSnapshot. Callers capture one
// before and one after a round/sweep/OOR cycle and feed both to
// AssertLedgerDelta.
//
// The snapshot is O(n) in total ledger size; test ledgers stay
// small so this is cheaper than adding a new admin surface.
func TakeLedgerSnapshot(ctx context.Context, tb testing.TB,
	client adminrpc.OperatorAdminClient) LedgerSnapshot {

	tb.Helper()

	snap := LedgerSnapshot{
		Balances:     make(map[string]int64, len(AllAccounts())),
		EventsByType: make(map[string]int),
	}
	for _, acc := range AllAccounts() {
		snap.Balances[acc] = 0
	}

	const pageSize = uint32(200)
	offset := uint32(0)
	for {
		resp, err := client.ListFeeEvents(
			ctx, &adminrpc.ListFeeEventsRequest{
				Limit:  pageSize,
				Offset: offset,
			},
		)
		require.NoError(tb, err, "ListFeeEvents")

		if len(resp.Events) == 0 {
			snap.TotalEntries = resp.Total
			break
		}

		for _, ev := range resp.Events {
			// Debit account: balance decreases.
			snap.Balances[ev.DebitAccount] -= ev.AmountSat

			// Credit account: balance increases.
			snap.Balances[ev.CreditAccount] += ev.AmountSat

			if ev.EntryId > snap.MaxEntryID {
				snap.MaxEntryID = ev.EntryId
			}

			snap.EventsByType[ev.EventType]++
		}

		offset += uint32(len(resp.Events))
		snap.TotalEntries = resp.Total
		if offset >= resp.Total {
			break
		}
	}

	return snap
}

// AssertLedgerDelta checks that the balance shift between two
// snapshots matches the caller's expectations. Every account
// in AllAccounts() is asserted: accounts missing from
// exp.Balances are asserted to have a zero delta (strong
// guarantee against unintended legs).
//
// The NewEntries and EventsByType fields are asserted similarly
// — any event_type missing from exp.EventsByType is asserted to
// have a zero count delta.
func AssertLedgerDelta(tb testing.TB, before, after LedgerSnapshot,
	exp ExpectedDelta) {

	tb.Helper()

	require.GreaterOrEqual(
		tb, after.MaxEntryID, before.MaxEntryID,
		"ledger entry IDs must be non-decreasing "+
			"(before=%d after=%d)",
		before.MaxEntryID, after.MaxEntryID,
	)

	require.Equal(
		tb, int(before.TotalEntries)+exp.NewEntries,
		int(after.TotalEntries),
		"unexpected new ledger entry count: got %d, want %d "+
			"(before=%d, expected new=%d)",
		int(after.TotalEntries)-int(before.TotalEntries),
		exp.NewEntries, before.TotalEntries, exp.NewEntries,
	)

	for _, acc := range AllAccounts() {
		got := after.Balances[acc] - before.Balances[acc]
		want := exp.Balances[acc]
		require.Equal(
			tb, want, got,
			fmt.Sprintf(
				"account %q balance delta: got "+
					"%d, want %d (before=%d "+
					"after=%d)",
				acc, got, want,
				before.Balances[acc],
				after.Balances[acc],
			),
		)
	}

	// Collect every event_type seen on either side so the
	// assertion surfaces unexpected new types as well.
	seen := make(map[string]struct{})
	for k := range before.EventsByType {
		seen[k] = struct{}{}
	}
	for k := range after.EventsByType {
		seen[k] = struct{}{}
	}
	for k := range exp.EventsByType {
		seen[k] = struct{}{}
	}

	for evType := range seen {
		got := after.EventsByType[evType] -
			before.EventsByType[evType]
		want := exp.EventsByType[evType]
		require.Equal(
			tb, want, got,
			fmt.Sprintf(
				"event_type %q count delta: got "+
					"%d, want %d",
				evType, got, want,
			),
		)
	}
}
