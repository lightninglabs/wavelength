//go:build walletdkrpc && swapruntime

package swapwallet

import (
	"context"
	"errors"
	"testing"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/lightninglabs/darepo-client/darepod"
	"github.com/lightninglabs/darepo-client/db"
	"github.com/lightninglabs/darepo-client/db/sqlc"
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

// trackForfeitedCooperativeExit tracks a wallet-local cooperative-leave EXIT
// whose retained VTXO outpoint is reported forfeited, so a reconcile pass
// decorates it COMPLETE. It returns the leave-job id.
func trackForfeitedCooperativeExit(runtime *Runtime,
	rpc *fakeRPCServer) string {

	const (
		jobID    = "sendjob-abc"
		outpoint = "aabbcc:0"
	)

	// A cooperative leave has no unroll job, and its retained outpoint is
	// in the forfeited set — so decorateExitEntry flips it to COMPLETE.
	rpc.unrollStatusResp = &daemonrpc.GetUnrollStatusResponse{Found: false}
	rpc.listVTXOsByStatus = map[daemonrpc.VTXOStatus]*daemonrpc.ListVTXOsResponse{
		daemonrpc.VTXOStatus_VTXO_STATUS_FORFEITED: {
			Vtxos: []*daemonrpc.VTXO{
				{
					Outpoint: outpoint,
				},
			},
		},
	}

	runtime.trackPendingEntryWithoutTimeout(&walletdkrpc.WalletEntry{
		Id:     jobID,
		Kind:   walletdkrpc.EntryKind_ENTRY_KIND_EXIT,
		Status: walletdkrpc.EntryStatus_ENTRY_STATUS_PENDING,
		Progress: &walletdkrpc.WalletEntryProgress{
			VtxoOutpoint: outpoint,
		},
	})

	return jobID
}

// TestReconcileClearsTerminalExitAfterProject verifies that once a
// cooperative-leave EXIT flips terminal, one reconcile pass durably projects
// the COMPLETE row AND clears the in-memory pending record — the clear happens
// after the successful project, not while decorating.
func TestReconcileClearsTerminalExitAfterProject(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	runtime, store, rpc := newReconcileFixture(t)

	jobID := trackForfeitedCooperativeExit(runtime, rpc)
	require.Contains(t, pendingSnapshotIDs(runtime), jobID)

	runtime.reconcileActivity(ctx)

	entry, err := store.GetEntry(ctx, jobID)
	require.NoError(t, err)
	require.EqualValues(
		t, walletdkrpc.EntryStatus_ENTRY_STATUS_COMPLETE, entry.Status,
		"reconciler must land the forfeited leave COMPLETE",
	)
	require.NotContains(
		t, pendingSnapshotIDs(runtime), jobID,
		"pending record must be cleared after a durable project",
	)
}

// TestReconcileRetainsPendingExitOnProjectFailure is the H-1 regression guard:
// when the terminal projection fails, the pending record must be retained so a
// later pass can retry, rather than stranding the row PENDING in the store with
// its only in-memory source destroyed.
func TestReconcileRetainsPendingExitOnProjectFailure(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	runtime, store, rpc := newReconcileFixture(t)

	// Wrap the store so every terminal projection fails.
	runtime.deps.ActivityStore = failingProjectStore{ActivityStore: store}

	jobID := trackForfeitedCooperativeExit(runtime, rpc)
	require.Contains(t, pendingSnapshotIDs(runtime), jobID)

	runtime.reconcileActivity(ctx)

	require.Contains(
		t, pendingSnapshotIDs(runtime), jobID, "a failed terminal "+
			"projection must not clear the pending record",
	)
}

// failingProjectStore wraps a real activity store but fails every ProjectEntry,
// to exercise the reconciler's must-not-clear-on-write-failure path.
type failingProjectStore struct {
	darepod.ActivityStore
}

func (failingProjectStore) ProjectEntry(context.Context,
	db.ActivityProjection) (int64, error) {

	return 0, errors.New("injected project failure")
}

// TestReconcileCompletesLeaveAfterRestart is the restart-survivability
// regression for cooperative-leave EXIT completion. It models a
// restart: the PENDING leave row is durably in the store but the in-memory
// pending map is empty (project writes only the store).
// rehydrateWalletLocalPending restores the correlation from the durable row so
// the reconciler flips the row COMPLETE under its stable send_job_id — the
// transition that was previously stranded PENDING forever after a mid-flight
// restart.
func TestReconcileCompletesLeaveAfterRestart(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	runtime, store, rpc := newReconcileFixture(t)

	const (
		jobID    = "sendjob-abc"
		outpoint = "aabbcc:0"
	)

	// The retained outpoint is reported forfeited (the round sealed) and
	// the leave has no unroll job, so the correlation flips it COMPLETE.
	rpc.unrollStatusResp = &daemonrpc.GetUnrollStatusResponse{Found: false}
	rpc.listVTXOsByStatus = map[daemonrpc.VTXOStatus]*daemonrpc.ListVTXOsResponse{
		daemonrpc.VTXOStatus_VTXO_STATUS_FORFEITED: {
			Vtxos: []*daemonrpc.VTXO{
				{
					Outpoint: outpoint,
				},
			},
		},
	}

	// Submit durably projected the PENDING row (keyed by send_job_id, with
	// the retained outpoint); a restart leaves the in-memory pending map
	// empty.
	runtime.project(ctx, &walletdkrpc.WalletEntry{
		Id:            jobID,
		Kind:          walletdkrpc.EntryKind_ENTRY_KIND_EXIT,
		Status:        walletdkrpc.EntryStatus_ENTRY_STATUS_PENDING,
		CreatedAtUnix: 100,
		UpdatedAtUnix: 100,
		Counterparty:  "cooperative",
		Progress: &walletdkrpc.WalletEntryProgress{
			VtxoOutpoint: outpoint,
		},
	})
	require.NotContains(
		t, pendingSnapshotIDs(runtime), jobID,
		"restart starts with an empty in-memory pending map",
	)

	// resumeAll rehydrates the pending map from the store. The fixture has
	// no SwapBackend, so this also asserts rehydration runs BEFORE the
	// swap- backend guard (and hence in degraded/test configs).
	runtime.resumeAll(ctx)
	require.Contains(
		t, pendingSnapshotIDs(runtime), jobID,
		"resumeAll must re-track the store's pending EXIT row",
	)

	// One reconcile pass now flips the stored row COMPLETE under
	// send_job_id.
	runtime.reconcileActivity(ctx)

	entry, err := store.GetEntry(ctx, jobID)
	require.NoError(t, err)
	require.EqualValues(
		t, walletdkrpc.EntryStatus_ENTRY_STATUS_COMPLETE, entry.Status,
		"rehydrated leave must complete under its stable id after "+
			"restart",
	)
	require.Equal(
		t, outpoint, entry.VtxoOutpoint,
		"the retained outpoint must survive the completion re-project",
	)
	require.NotContains(
		t, pendingSnapshotIDs(runtime), jobID,
		"the pending record is cleared after the durable project",
	)
}

// TestRehydrateWalletLocalPending verifies startup rehydration re-tracks only
// wallet-local PENDING EXIT rows: PENDING EXITs (both send_job_id- and
// outpoint-keyed) are restored; a COMPLETE EXIT and a PENDING SEND are not.
func TestRehydrateWalletLocalPending(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	runtime, _, _ := newReconcileFixture(t)

	seed := func(id string, kind walletdkrpc.EntryKind,
		status walletdkrpc.EntryStatus) {

		runtime.project(ctx, &walletdkrpc.WalletEntry{
			Id:            id,
			Kind:          kind,
			Status:        status,
			CreatedAtUnix: 100,
			UpdatedAtUnix: 100,
		})
	}
	seed(
		"sendjob-abc", walletdkrpc.EntryKind_ENTRY_KIND_EXIT,
		walletdkrpc.EntryStatus_ENTRY_STATUS_PENDING,
	)
	seed(
		"ddeeff:0", walletdkrpc.EntryKind_ENTRY_KIND_EXIT,
		walletdkrpc.EntryStatus_ENTRY_STATUS_PENDING,
	)
	seed(
		"sendjob-done", walletdkrpc.EntryKind_ENTRY_KIND_EXIT,
		walletdkrpc.EntryStatus_ENTRY_STATUS_COMPLETE,
	)
	seed(
		"payhash-send", walletdkrpc.EntryKind_ENTRY_KIND_SEND,
		walletdkrpc.EntryStatus_ENTRY_STATUS_PENDING,
	)

	// project writes only the store; the in-memory map starts empty.
	require.Empty(t, pendingSnapshotIDs(runtime))

	runtime.rehydrateWalletLocalPending(ctx)

	ids := pendingSnapshotIDs(runtime)
	require.Contains(t, ids, "sendjob-abc")
	require.Contains(t, ids, "ddeeff:0")
	require.NotContains(
		t, ids, "sendjob-done", "COMPLETE rows must not be re-tracked",
	)
	require.NotContains(
		t, ids, "payhash-send", "SEND rows must not be re-tracked",
	)
}

// TestRehydratePagesMultipleBatches verifies the scan advances its canonical_id
// cursor across pages and terminates: with a page size of 2 and five PENDING
// EXIT rows it restores all five exactly once (three pages: 2, 2, 1).
func TestRehydratePagesMultipleBatches(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	runtime, _, _ := newReconcileFixture(t)
	runtime.rehydratePageSize = 2

	ids := []string{"exit-a", "exit-b", "exit-c", "exit-d", "exit-e"}
	for _, id := range ids {
		runtime.project(ctx, &walletdkrpc.WalletEntry{
			Id:            id,
			Kind:          walletdkrpc.EntryKind_ENTRY_KIND_EXIT,
			Status:        walletdkrpc.EntryStatus_ENTRY_STATUS_PENDING,
			CreatedAtUnix: 100,
			UpdatedAtUnix: 100,
		})
	}

	runtime.rehydrateWalletLocalPending(ctx)

	got := pendingSnapshotIDs(runtime)
	for _, id := range ids {
		require.Contains(
			t, got, id,
			"every pending EXIT must be restored across pages",
		)
	}
	require.Len(t, got, len(ids), "no row restored more than once")
}

// TestRehydrateReturnsOnScanError verifies a scan error aborts rehydration
// cleanly (no panic, empty map) rather than partially populating it.
func TestRehydrateReturnsOnScanError(t *testing.T) {
	t.Parallel()

	runtime, store, _ := newReconcileFixture(t)
	runtime.deps.ActivityStore = erringRehydrateStore{ActivityStore: store}

	require.NotPanics(t, func() {
		runtime.rehydrateWalletLocalPending(context.Background())
	})
	require.Empty(t, pendingSnapshotIDs(runtime))
}

// erringRehydrateStore fails only the rehydration scan, delegating every other
// method to a real store.
type erringRehydrateStore struct {
	darepod.ActivityStore
}

func (erringRehydrateStore) ListEntriesByKindStatus(context.Context, int64,
	int64, string, int32) ([]sqlc.ActivityEntry, error) {

	return nil, errors.New("injected scan failure")
}
