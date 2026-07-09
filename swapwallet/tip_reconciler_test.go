//go:build walletdkrpc && swapruntime

package swapwallet

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/lightninglabs/darepo-client/db"
	"github.com/lightninglabs/darepo-client/ledger"
	"github.com/lightninglabs/darepo-client/rpc/walletdkrpc"
	"github.com/stretchr/testify/require"
)

// seedPendingDepositThatConfirms seeds a PENDING deposit row and points
// ListTransactions at the confirmed boarding row for the same address, so one
// reconcile pass flips the stored row to COMPLETE. Mirrors the reconciler
// deposit-flip fixture.
func seedPendingDepositThatConfirms(t *testing.T, runtime *Runtime,
	rpc *fakeRPCServer) string {

	t.Helper()

	const depID = "deposit-bcrt1qaddr"
	runtime.project(context.Background(), &walletdkrpc.WalletEntry{
		Id:            depID,
		Kind:          walletdkrpc.EntryKind_ENTRY_KIND_DEPOSIT,
		Status:        walletdkrpc.EntryStatus_ENTRY_STATUS_PENDING,
		CreatedAtUnix: 100,
		UpdatedAtUnix: 100,
		Progress: &walletdkrpc.WalletEntryProgress{
			PhaseLabel: "address_issued",
		},
	})

	rpc.listTxResp = &daemonrpc.ListTransactionsResponse{
		Transactions: []*daemonrpc.TransactionHistoryEntry{{
			Type:               "boarding",
			Subtype:            ledger.EventWalletUTXOCreated,
			ConfirmationStatus: "confirmed",
			AmountSat:          100_000,
			Txid:               "boarding-txid",
			OutputIndex:        0,
			BoardingAddress:    "bcrt1qaddr",
			CreatedAtUnixS:     100,
		}},
	}

	return depID
}

// depositComplete reports whether the stored deposit row has reached COMPLETE.
func depositComplete(store *db.ActivityPersistenceStore, id string) bool {
	entry, err := store.GetEntry(context.Background(), id)

	return err == nil && walletdkrpc.EntryStatus(entry.Status) ==
		walletdkrpc.EntryStatus_ENTRY_STATUS_COMPLETE
}

// TestTipReconcileFiresWhenTipSettles verifies the startup loop runs one
// reconcile pass once the best-block height is stable across a poll interval
// and the wallet is READY, flipping a deposit that confirmed while down.
func TestTipReconcileFiresWhenTipSettles(t *testing.T) {
	t.Parallel()

	runtime, store, rpc := newReconcileFixture(t)
	depID := seedPendingDepositThatConfirms(t, runtime, rpc)

	// A settled tip: a fixed best-block height (stable across polls) and a
	// READY wallet.
	rpc.getInfoResp = &daemonrpc.GetInfoResponse{
		BlockHeight: 100,
		WalletState: daemonrpc.WalletState_WALLET_STATE_READY,
	}
	runtime.tipPollInterval = time.Millisecond

	runtime.startTipReconcileLoop()

	require.Eventually(t, func() bool {
		return depositComplete(store, depID)
	}, 5*time.Second, 5*time.Millisecond,
		"tip reconcile did not flip the confirmed deposit")
}

// TestTipReconcileBackstopReconcilesOnTimeout verifies that when the best block
// never settles (height keeps advancing), the timeout backstop still runs one
// reconcile pass and hands off to the periodic reconciler.
func TestTipReconcileBackstopReconcilesOnTimeout(t *testing.T) {
	t.Parallel()

	runtime, store, rpc := newReconcileFixture(t)
	depID := seedPendingDepositThatConfirms(t, runtime, rpc)

	// The best block advances on every poll, so the stability heuristic
	// never triggers; only the timeout backstop can reconcile.
	var height atomic.Uint32
	rpc.getInfoHook = func(_ int) (*daemonrpc.GetInfoResponse, error) {
		return &daemonrpc.GetInfoResponse{
			BlockHeight: height.Add(1),
			WalletState: daemonrpc.WalletState_WALLET_STATE_READY,
		}, nil
	}
	runtime.tipPollInterval = time.Millisecond
	runtime.tipReconcileTimeout = 30 * time.Millisecond

	runtime.startTipReconcileLoop()

	require.Eventually(t, func() bool {
		return depositComplete(store, depID)
	}, 5*time.Second, 5*time.Millisecond,
		"timeout backstop did not reconcile")
}

// TestTipReconcileWaitsWhileSyncing verifies the loop does not reconcile while
// the wallet is still SYNCING even if the height is stable: it must wait for
// READY (the backstop timeout is set far out so it cannot fire in-window).
func TestTipReconcileWaitsWhileSyncing(t *testing.T) {
	t.Parallel()

	runtime, store, rpc := newReconcileFixture(t)
	depID := seedPendingDepositThatConfirms(t, runtime, rpc)

	rpc.getInfoResp = &daemonrpc.GetInfoResponse{
		BlockHeight: 100,
		WalletState: daemonrpc.WalletState_WALLET_STATE_SYNCING,
	}
	runtime.tipPollInterval = time.Millisecond
	runtime.tipReconcileTimeout = 10 * time.Second

	runtime.startTipReconcileLoop()

	require.Never(t, func() bool {
		return depositComplete(store, depID)
	}, 200*time.Millisecond, 10*time.Millisecond,
		"tip reconcile must wait until the wallet is READY")
}

// TestTipReconcileNoDepsIsNoOp verifies the loop is a safe no-op without a
// canonical store or without an RPCServer to poll.
func TestTipReconcileNoDepsIsNoOp(t *testing.T) {
	t.Parallel()

	require.NotPanics(t, func() {
		noStore := newRuntime(
			t.Context(), &Deps{
				RPCServer: &fakeRPCServer{},
			},
		)
		t.Cleanup(noStore.stop)
		noStore.startTipReconcileLoop()

		noRPC := newRuntime(t.Context(), &Deps{})
		t.Cleanup(noRPC.stop)
		noRPC.startTipReconcileLoop()
	})
}
