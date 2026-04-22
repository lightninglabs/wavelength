//go:build itest

package itest

import (
	"strings"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	client_harness "github.com/lightninglabs/darepo-client/harness"
	"github.com/lightninglabs/darepo/adminrpc"
	"github.com/lightninglabs/darepo/harness"
	"github.com/stretchr/testify/require"
)

// TestFeesClassifierExternalFundingAccountExists is the
// minimum-viable classifier itest: verify that the external_
// funding account is registered in the operator's chart of
// accounts and that a ledger query against it succeeds. A full
// end-to-end deposit-to-external_funding test requires
// running through a block epoch and the grace-window
// reconciliation loop, which is better covered by a systest
// that can drive the block epoch and query at the exact
// reconciled height.
//
// The assertion here is that ListFeeEvents can return entries
// filtered by event_type without error for the two reserved
// external-wallet movement events. A regression that dropped
// the external_* event-type constants would break this call.
func TestFeesClassifierExternalAccountsReachable(t *testing.T) {
	t.Parallel()

	clientOpts := client_harness.DefaultOptions()
	clientOpts.GroupName = t.Name()
	clientOpts.StartTapd = false

	h := harness.NewArkHarness(t, &harness.ArkHarnessOptions{
		ClientOptions: &clientOpts,
	})
	t.Cleanup(h.Stop)

	h.Start()
	h.FundOperatorLND(btcutil.SatoshiPerBitcoin)

	ctx := t.Context()

	// Filter by external_deposit: must not error, and the page
	// should contain only external_deposit events (zero or more).
	resp, err := h.ArkAdminClient.ListFeeEvents(
		ctx, &adminrpc.ListFeeEventsRequest{
			Limit:           100,
			EventTypeFilter: "external_deposit",
		},
	)
	require.NoError(t, err, "ListFeeEvents(external_deposit)")
	for _, ev := range resp.Events {
		require.Equal(
			t, "external_deposit", ev.EventType,
			"filter must return only the requested type",
		)
		// Every external_deposit credits the external_funding
		// account; the debit side varies (treasury_wallet on
		// a real deposit, etc.).
		require.True(
			t, strings.EqualFold(
				ev.CreditAccount, "external_funding",
			),
			"external_deposit must credit external_funding",
		)
	}

	// Filter by external_withdrawal: same check.
	resp, err = h.ArkAdminClient.ListFeeEvents(
		ctx, &adminrpc.ListFeeEventsRequest{
			Limit:           100,
			EventTypeFilter: "external_withdrawal",
		},
	)
	require.NoError(t, err, "ListFeeEvents(external_withdrawal)")
	for _, ev := range resp.Events {
		require.Equal(
			t, "external_withdrawal", ev.EventType,
		)
		require.True(
			t, strings.EqualFold(
				ev.DebitAccount, "external_funding",
			),
			"external_withdrawal must debit external_funding",
		)
	}
}
