//go:build walletdkrpc && swapruntime

package swapwallet

import (
	"context"
	"errors"
	"testing"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/db/sqlc"
	"github.com/lightninglabs/wavelength/ledger"
	"github.com/lightninglabs/wavelength/rpc/swapclientrpc"
	"github.com/lightninglabs/wavelength/rpc/walletdkrpc"
	"github.com/lightninglabs/wavelength/waved"
	"github.com/lightninglabs/wavelength/waverpc"
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
	rpc.listTxResp = &waverpc.ListTransactionsResponse{
		Transactions: []*waverpc.TransactionHistoryEntry{
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

// TestReconcileProjectsRawOORLive verifies the reconciler lands a raw
// out-of-round SEND/RECV row into the store live. A raw `ark send oor` / `ark
// oor receive` is neither swap-backed nor credit-backed, so it has no live
// projector; before SEND/RECV were added to reconcilerKinds its row reached the
// store only at the next startup backfill (issue #903). The rows carry no swap
// session correlation (SessionId unset), matching a raw OOR that the
// swap/credit projectors never own — the derivation of correlated OOR rows is
// covered by the history tests.
func TestReconcileProjectsRawOORLive(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	runtime, store, rpc := newReconcileFixture(t)

	// A recorded raw-OOR send (debit transfers_out) and receive (credit
	// transfers_in) as ListTransactions surfaces them. statusForLedgerRow
	// folds an "oor"/"recorded" row to COMPLETE.
	rpc.listTxResp = &waverpc.ListTransactionsResponse{
		Transactions: []*waverpc.TransactionHistoryEntry{
			{
				Type:               "oor",
				ConfirmationStatus: "recorded",
				DebitAccount:       ledger.AccountTransfersOut,
				AmountSat:          5_000,
				Txid:               "oor-send-txid",
				CreatedAtUnixS:     200,
			},
			{
				Type:               "oor",
				ConfirmationStatus: "recorded",
				CreditAccount:      ledger.AccountTransfersIn,
				AmountSat:          3_000,
				Txid:               "oor-recv-txid",
				CreatedAtUnixS:     201,
			},
		},
	}

	// One reconcile pass projects both rows into the store live.
	runtime.reconcileActivity(ctx)

	send, err := store.GetEntry(ctx, "oor-send-txid")
	require.NoError(t, err)
	require.EqualValues(
		t, walletdkrpc.EntryKind_ENTRY_KIND_SEND, send.Kind,
	)
	require.EqualValues(
		t, walletdkrpc.EntryStatus_ENTRY_STATUS_COMPLETE, send.Status,
		"reconciler must land the raw-OOR send live",
	)

	recv, err := store.GetEntry(ctx, "oor-recv-txid")
	require.NoError(t, err)
	require.EqualValues(
		t, walletdkrpc.EntryKind_ENTRY_KIND_RECV, recv.Kind,
	)
	require.EqualValues(
		t, walletdkrpc.EntryStatus_ENTRY_STATUS_COMPLETE, recv.Status,
		"reconciler must land the raw-OOR receive live",
	)
}

// TestReprojectRecentActivityBoundsWindow verifies the raw-OOR reconcile pass
// projects only the most recent `limit` rows and skips older ones, so the
// per-pass ProjectEntry work stays bounded at O(limit) rather than scanning the
// unbounded SEND/RECV history (the review concern behind
// rawOORReconcileWindow).
func TestReprojectRecentActivityBoundsWindow(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	runtime, store, rpc := newReconcileFixture(t)

	// Three recorded raw-OOR sends with increasing updated_at (ledger rows
	// take updated_at from created_at). deriveActivity sorts updated_at
	// descending, so with limit 2 only the two newest are projected.
	rpc.listTxResp = &waverpc.ListTransactionsResponse{
		Transactions: []*waverpc.TransactionHistoryEntry{
			{
				Type:               "oor",
				ConfirmationStatus: "recorded",
				DebitAccount:       ledger.AccountTransfersOut,
				AmountSat:          1_000,
				Txid:               "oor-old",
				CreatedAtUnixS:     100,
			},
			{
				Type:               "oor",
				ConfirmationStatus: "recorded",
				DebitAccount:       ledger.AccountTransfersOut,
				AmountSat:          2_000,
				Txid:               "oor-mid",
				CreatedAtUnixS:     200,
			},
			{
				Type:               "oor",
				ConfirmationStatus: "recorded",
				DebitAccount:       ledger.AccountTransfersOut,
				AmountSat:          3_000,
				Txid:               "oor-new",
				CreatedAtUnixS:     300,
			},
		},
	}

	const window = 2
	n, err := runtime.reprojectRecentActivity(
		ctx, rawOORReconcileKinds, window,
	)
	require.NoError(t, err)
	require.Equal(t, window, n, "only the window's worth of rows projected")

	// The two newest landed; the oldest was outside the window.
	_, err = store.GetEntry(ctx, "oor-new")
	require.NoError(t, err)
	_, err = store.GetEntry(ctx, "oor-mid")
	require.NoError(t, err)

	_, err = store.GetEntry(ctx, "oor-old")
	require.Error(t, err, "row beyond the window must not be projected")
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
	rpc.unrollStatusResp = &waverpc.GetUnrollStatusResponse{Found: false}
	rpc.listVTXOsByStatus = map[waverpc.VTXOStatus]*waverpc.ListVTXOsResponse{
		waverpc.VTXOStatus_VTXO_STATUS_FORFEITED: {
			Vtxos: []*waverpc.VTXO{
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
	waved.ActivityStore
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
	rpc.unrollStatusResp = &waverpc.GetUnrollStatusResponse{Found: false}
	rpc.listVTXOsByStatus = map[waverpc.VTXOStatus]*waverpc.ListVTXOsResponse{
		waverpc.VTXOStatus_VTXO_STATUS_FORFEITED: {
			Vtxos: []*waverpc.VTXO{
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
	waved.ActivityStore
}

func (erringRehydrateStore) ListEntriesByKindStatus(context.Context, int64,
	int64, string, int32) ([]sqlc.ActivityEntry, error) {

	return nil, errors.New("injected scan failure")
}
