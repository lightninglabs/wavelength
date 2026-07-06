//go:build walletdkrpc && swapruntime

package swapwallet

import (
	"context"
	"testing"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/lightninglabs/darepo-client/db"
	"github.com/lightninglabs/darepo-client/ledger"
	"github.com/lightninglabs/darepo-client/rpc/swapclientrpc"
	"github.com/lightninglabs/darepo-client/rpc/walletdkrpc"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/stretchr/testify/require"
)

// newReconcileFixture wires a Runtime over a real activity store plus fake RPC
// and swap services, so the reconciler's re-derive-and-project pass can be
// exercised end to end against the store.
func newReconcileFixture(t *testing.T) (*Runtime, *db.ActivityPersistenceStore,
	*fakeRPCServer) {

	t.Helper()

	testDB := db.NewTestDB(t)
	store := db.NewStore(
		testDB.DB, testDB.Queries, testDB.Backend(), btclog.Disabled,
	).NewActivityStore(clock.NewDefaultClock())

	rpc := &fakeRPCServer{}
	swap := &fakeSwapService{
		listSwapsResp: &swapclientrpc.ListSwapsResponse{},
	}
	deps := &Deps{ActivityStore: store, RPCServer: rpc, SwapService: swap}
	runtime := newRuntime(t.Context(), deps)
	t.Cleanup(runtime.stop)

	return runtime, store, rpc
}

// TestReconcileActivityFlipsDepositLive verifies the reconciler lands a
// confirmed boarding deposit's PENDING -> COMPLETE transition into the store
// live (no restart), and that a second pass is a no-op (ProjectEntry
// change-suppression) appending no duplicate transition event.
func TestReconcileActivityFlipsDepositLive(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	runtime, store, rpc := newReconcileFixture(t)

	// A pending deposit row already in the store, as Deposit would project
	// it, keyed by its address-scoped id.
	const depID = "deposit-bcrt1qaddr"
	runtime.project(ctx, &walletdkrpc.WalletEntry{
		Id:            depID,
		Kind:          walletdkrpc.EntryKind_ENTRY_KIND_DEPOSIT,
		Status:        walletdkrpc.EntryStatus_ENTRY_STATUS_PENDING,
		CreatedAtUnix: 100,
		UpdatedAtUnix: 100,
		Progress: &walletdkrpc.WalletEntryProgress{
			PhaseLabel: "address_issued",
		},
	})

	entry, err := store.GetEntry(ctx, depID)
	require.NoError(t, err)
	require.EqualValues(
		t, walletdkrpc.EntryStatus_ENTRY_STATUS_PENDING, entry.Status,
	)

	// The deposit confirms: ListTransactions now returns the confirmed
	// wallet_utxo_created row carrying the same boarding address, so it
	// keys to the same deposit-<address> id as the pending row.
	rpc.listTxResp = &daemonrpc.ListTransactionsResponse{
		Transactions: []*daemonrpc.TransactionHistoryEntry{
			{
				Type:               "boarding",
				Subtype:            ledger.EventWalletUTXOCreated,
				ConfirmationStatus: "confirmed",
				AmountSat:          100_000,
				Txid:               "boarding-txid",
				OutputIndex:        0,
				BoardingAddress:    "bcrt1qaddr",
				CreatedAtUnixS:     100,
			},
		},
	}

	// One reconcile pass flips the stored row to COMPLETE.
	runtime.reconcileActivity(ctx)

	entry, err = store.GetEntry(ctx, depID)
	require.NoError(t, err)
	require.EqualValues(
		t, walletdkrpc.EntryStatus_ENTRY_STATUS_COMPLETE, entry.Status,
		"reconciler must land the confirmed deposit live",
	)

	events, err := store.PullEvents(ctx, 0, 100)
	require.NoError(t, err)
	settled := len(events)

	// A second pass is a no-op: no duplicate transition event.
	runtime.reconcileActivity(ctx)
	events, err = store.PullEvents(ctx, 0, 100)
	require.NoError(t, err)
	require.Len(
		t, events, settled, "re-reconcile must append no new event",
	)
}

// TestReconcileActivityNoStoreIsNoOp verifies the reconciler is a safe no-op
// without a canonical store: the loop is never started and a direct pass does
// not panic.
func TestReconcileActivityNoStoreIsNoOp(t *testing.T) {
	t.Parallel()

	deps := &Deps{}
	runtime := newRuntime(t.Context(), deps)
	t.Cleanup(runtime.stop)

	require.NotPanics(t, func() {
		runtime.startReconcilerLoop()
		runtime.reconcileActivity(context.Background())
	})
}
