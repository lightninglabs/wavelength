//go:build wavewalletrpc && swapruntime

package swapwallet

import (
	"testing"

	"github.com/lightninglabs/wavelength/credit"
	"github.com/lightninglabs/wavelength/rpc/wavewalletrpc"
	"github.com/stretchr/testify/require"
)

// payHashHex is an arbitrary 32-byte payment-hash hex; the projector only
// strips the "pay:" op-key prefix, so the exact value is immaterial.
const payHashHex = "00112233445566778899aabbccddeeff" +
	"00112233445566778899aabbccddeeff"

// drainEntries non-blockingly collects every WalletEntry currently queued on a
// subscriber.
func drainEntries(sub *subscriber) []*wavewalletrpc.WalletEntry {
	var out []*wavewalletrpc.WalletEntry
	for {
		select {
		case u := <-sub.ch:
			out = append(out, u.entry)

		default:
			return out
		}
	}
}

// indexEntriesByID indexes entries by their id for assertion lookups.
func indexEntriesByID(
	entries []*wavewalletrpc.WalletEntry) map[string]*wavewalletrpc.WalletEntry {

	out := make(map[string]*wavewalletrpc.WalletEntry, len(entries))
	for _, e := range entries {
		out[e.GetId()] = e
	}

	return out
}

// TestCreditProjectorProjectsOwnedTerminals asserts the projector emits a
// terminal WalletEntry for the operations it owns — credit-only pays (keyed by
// payment hash) and credit receives (keyed by op id) — and stays silent for
// mixed pays (owned by the swap monitor) and redemptions (wallet-internal).
func TestCreditProjectorProjectsOwnedTerminals(t *testing.T) {
	t.Parallel()

	reg := &fakeCreditRegistry{
		listResp: &credit.ListCreditOpsResponse{
			Ops: []credit.CreditOpSummary{
				{
					OpID:       "op-pay",
					OpKey:      "pay:" + payHashHex,
					Kind:       credit.KindPay,
					State:      credit.StateCompleted,
					CreditOnly: true,
					AmountSat:  500,
				},
				{
					OpID:      "op-recv",
					OpKey:     "recv:xyz",
					Kind:      credit.KindReceive,
					State:     credit.StateCompleted,
					AmountSat: 42,
				},
				{
					OpID:       "op-mixed",
					OpKey:      "pay:beefbeef",
					Kind:       credit.KindPay,
					State:      credit.StateCompleted,
					CreditOnly: false,
					AmountSat:  1000,
				},
				{
					OpID:      "op-redeem",
					OpKey:     "redeem:r",
					Kind:      credit.KindRedeem,
					State:     credit.StateCompleted,
					AmountSat: 9,
				},
			},
		},
	}
	deps := &Deps{CreditRegistry: reg}
	runtime := newRuntime(t.Context(), deps)
	t.Cleanup(runtime.stop)

	ch := runtime.subscribe()
	projected := make(map[string]credit.State)
	runtime.pollCreditOps(projected)

	got := drainEntries(ch)
	require.Len(t, got, 2)
	byID := indexEntriesByID(got)

	// Credit-only pay -> SEND COMPLETE keyed by the payment-hash hex.
	pay := byID[payHashHex]
	require.NotNil(t, pay)
	require.Equal(
		t, wavewalletrpc.EntryKind_ENTRY_KIND_SEND, pay.GetKind(),
	)
	require.Equal(
		t, wavewalletrpc.EntryStatus_ENTRY_STATUS_COMPLETE,
		pay.GetStatus(),
	)
	require.Equal(t, int64(-500), pay.GetAmountSat())
	require.Equal(
		t, wavewalletrpc.WalletEntryPhase_WALLET_ENTRY_PHASE_CONFIRMED,
		pay.GetProgress().GetPhase(),
	)

	// Receive -> RECV COMPLETE keyed by the op id.
	recv := byID["op-recv"]
	require.NotNil(t, recv)
	require.Equal(
		t, wavewalletrpc.EntryKind_ENTRY_KIND_RECV, recv.GetKind(),
	)
	require.Equal(
		t, wavewalletrpc.EntryStatus_ENTRY_STATUS_COMPLETE,
		recv.GetStatus(),
	)
	require.Equal(t, int64(42), recv.GetAmountSat())

	// Mixed pay and redeem are not projected.
	require.Nil(t, byID["beefbeef"])
	require.Nil(t, byID["op-mixed"])
	require.Nil(t, byID["op-redeem"])

	// A second poll with unchanged state emits nothing.
	runtime.pollCreditOps(projected)
	require.Empty(t, drainEntries(ch))
}

// TestCreditProjectorWritesToStore asserts the projector persists the credit
// rows it owns into the canonical activity store (not only emits them), so
// credit-only sends are in the store before the read path cuts over to it. A
// re-poll of unchanged state projects nothing further.
func TestCreditProjectorWritesToStore(t *testing.T) {
	t.Parallel()

	reg := &fakeCreditRegistry{
		listResp: &credit.ListCreditOpsResponse{
			Ops: []credit.CreditOpSummary{
				{
					OpID:       "op-pay",
					OpKey:      "pay:" + payHashHex,
					Kind:       credit.KindPay,
					State:      credit.StateCompleted,
					CreditOnly: true,
					AmountSat:  500,
				},
				{
					OpID:      "op-recv",
					OpKey:     "recv:xyz",
					Kind:      credit.KindReceive,
					State:     credit.StateCompleted,
					AmountSat: 42,
				},
			},
		},
	}
	store := &fakeActivityProjector{}
	deps := &Deps{CreditRegistry: reg, ActivityStore: store}
	runtime := newRuntime(t.Context(), deps)
	t.Cleanup(runtime.stop)

	projected := make(map[string]credit.State)
	runtime.pollCreditOps(projected)

	require.Equal(t, 2, store.count())
	ids := store.ids()
	require.True(t, ids[payHashHex], "credit-only pay projected by hash")
	require.True(t, ids["op-recv"], "credit receive projected by op id")

	// A second poll with unchanged state projects nothing further.
	runtime.pollCreditOps(projected)
	require.Equal(t, 2, store.count())
}

// TestCreditProjectorProjectsFailure asserts a failed credit op surfaces as a
// FAILED WalletEntry carrying the operation's terminal error.
func TestCreditProjectorProjectsFailure(t *testing.T) {
	t.Parallel()

	reg := &fakeCreditRegistry{
		listResp: &credit.ListCreditOpsResponse{
			Ops: []credit.CreditOpSummary{
				{
					OpID:      "op-recv",
					OpKey:     "recv:z",
					Kind:      credit.KindReceive,
					State:     credit.StateFailed,
					AmountSat: 7,
					LastError: "receive funding ended in FAILED",
				},
			},
		},
	}
	deps := &Deps{CreditRegistry: reg}
	runtime := newRuntime(t.Context(), deps)
	t.Cleanup(runtime.stop)

	ch := runtime.subscribe()
	runtime.pollCreditOps(make(map[string]credit.State))

	got := drainEntries(ch)
	require.Len(t, got, 1)
	entry := got[0]
	require.Equal(t, "op-recv", entry.GetId())
	require.Equal(
		t, wavewalletrpc.EntryStatus_ENTRY_STATUS_FAILED,
		entry.GetStatus(),
	)
	require.Equal(
		t, "receive funding ended in FAILED", entry.GetFailureReason(),
	)
	require.Equal(
		t, wavewalletrpc.EntryFailureCode_ENTRY_FAILURE_CODE_FAILED,
		entry.GetFailureCode(),
	)
}

// TestCreditProjectorTracksPendingForRestart asserts an in-flight credit-only
// op is re-tracked as a wallet-local pending row so it survives in List
// snapshots even though the runtime pending map is in-memory only.
func TestCreditProjectorTracksPendingForRestart(t *testing.T) {
	t.Parallel()

	reg := &fakeCreditRegistry{
		listResp: &credit.ListCreditOpsResponse{
			Ops: []credit.CreditOpSummary{
				{
					OpID:       "op-pay",
					OpKey:      "pay:" + payHashHex,
					Kind:       credit.KindPay,
					State:      credit.StatePaying,
					CreditOnly: true,
					AmountSat:  500,
				},
			},
		},
	}
	deps := &Deps{CreditRegistry: reg}
	runtime := newRuntime(t.Context(), deps)
	t.Cleanup(runtime.stop)

	runtime.pollCreditOps(make(map[string]credit.State))

	snapshot := runtime.pendingSnapshot()
	require.Len(t, snapshot, 1)
	require.Equal(t, payHashHex, snapshot[0].GetId())
	require.Equal(
		t, wavewalletrpc.EntryStatus_ENTRY_STATUS_PENDING,
		snapshot[0].GetStatus(),
	)
}
