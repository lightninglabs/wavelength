//go:build wavewalletrpc && swapruntime

package swapwallet

import (
	"context"
	"testing"

	"github.com/lightninglabs/wavelength/credit"
	"github.com/lightninglabs/wavelength/rpc/swapclientrpc"
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
	require.True(
		t,
		runtime.creditProjectorOwnsSwapSummary(
			&swapclientrpc.SwapSummary{
				PaymentHash: payHashHex,
				SettlementType: swapclientrpc.
					SwapSettlementType_SWAP_SETTLEMENT_TYPE_CREDIT,
			},
		),
	)

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

// TestResumeAllRestoresCreditOwnershipBeforeMonitor verifies the wallet-ready
// resume phase rebuilds durable credit ownership before startup backfill and
// live monitoring can derive a zero-amount SDK swap row.
func TestResumeAllRestoresCreditOwnershipBeforeMonitor(t *testing.T) {
	t.Parallel()

	reg := &fakeCreditRegistry{
		listResp: &credit.ListCreditOpsResponse{
			Ops: []credit.CreditOpSummary{{
				OpID:       "op-restart-pay",
				OpKey:      "pay:" + payHashHex,
				Kind:       credit.KindPay,
				State:      credit.StateCompleted,
				CreditOnly: true,
			}},
		},
	}
	runtime := newRuntime(t.Context(), &Deps{CreditRegistry: reg})
	t.Cleanup(runtime.stop)

	runtime.resumeAll(t.Context())

	require.True(
		t,
		runtime.creditProjectorOwnsSwapSummary(
			&swapclientrpc.SwapSummary{
				PaymentHash: payHashHex,
				SettlementType: swapclientrpc.
					SwapSettlementType_SWAP_SETTLEMENT_TYPE_CREDIT,
			},
		),
	)
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

// TestCreditProjectorReopensLegacyReceiveActivity proves the upgrade repair is
// visible to users: a canonical FAILED row carrying the exact old poll-cap
// error becomes PENDING with a corrective event, retains its invoice context,
// and can later advance to COMPLETE. The global terminal-to-pending rule stays
// unchanged for every other activity row.
func TestCreditProjectorReopensLegacyReceiveActivity(t *testing.T) {
	t.Parallel()

	history, store := newStoreListFixture(t)
	runtime := history.runtime

	const opID = "legacy-receive"
	failed := &wavewalletrpc.WalletEntry{
		Id:            opID,
		Kind:          wavewalletrpc.EntryKind_ENTRY_KIND_RECV,
		Status:        wavewalletrpc.EntryStatus_ENTRY_STATUS_FAILED,
		AmountSat:     200,
		Counterparty:  creditCounterparty,
		Note:          "small receive",
		FailureReason: credit.LegacyReceivePollCapError,
		FailureCode: wavewalletrpc.
			EntryFailureCode_ENTRY_FAILURE_CODE_FAILED.Enum(),
		Request: &wavewalletrpc.WalletEntryRequest{
			Request: &wavewalletrpc.
				WalletEntryRequest_LightningInvoice{
				LightningInvoice: &wavewalletrpc.
					LightningInvoiceRequest{
					Invoice: "lnbc-legacy",
				},
			},
		},
		Progress: &wavewalletrpc.WalletEntryProgress{
			Phase: wavewalletrpc.
				WalletEntryPhase_WALLET_ENTRY_PHASE_FAILED,
			PhaseLabel: "failed",
		},
	}
	runtime.projectAndEmit(t.Context(), failed)

	op := credit.CreditOpSummary{
		OpID:      opID,
		OpKey:     "recv:legacy",
		Kind:      credit.KindReceive,
		State:     credit.StateAwaitingSettlement,
		Pending:   true,
		AmountSat: 200,
	}
	reg := &fakeCreditRegistry{
		listResp: &credit.ListCreditOpsResponse{
			Ops: []credit.CreditOpSummary{
				op,
			},
		},
	}
	runtime.deps.CreditRegistry = reg

	sub := runtime.subscribe()
	t.Cleanup(func() { runtime.unsubscribe(sub) })
	projected := make(map[string]credit.State)
	runtime.pollCreditOps(projected)

	row, err := store.GetEntry(t.Context(), opID)
	require.NoError(t, err)
	require.EqualValues(
		t, wavewalletrpc.EntryStatus_ENTRY_STATUS_PENDING, row.Status,
	)
	require.Empty(t, row.FailureReason)
	require.Equal(t, "small receive", row.Note)
	require.Contains(t, row.RequestJson, "lnbc-legacy")

	updates := drainEntries(sub)
	require.Len(t, updates, 1)
	require.Equal(
		t, wavewalletrpc.EntryStatus_ENTRY_STATUS_PENDING,
		updates[0].GetStatus(),
	)
	require.Equal(
		t, "lnbc-legacy",
		updates[0].GetRequest().GetLightningInvoice().GetInvoice(),
	)

	events, err := store.PullEvents(t.Context(), 0, 10)
	require.NoError(t, err)
	require.Len(t, events, 2)
	require.EqualValues(
		t, wavewalletrpc.EntryStatus_ENTRY_STATUS_PENDING,
		events[1].Status,
	)

	op.State = credit.StateCompleted
	op.Pending = false
	reg.listResp.Ops[0] = op
	runtime.pollCreditOps(projected)

	row, err = store.GetEntry(t.Context(), opID)
	require.NoError(t, err)
	require.EqualValues(
		t, wavewalletrpc.EntryStatus_ENTRY_STATUS_COMPLETE, row.Status,
	)
}

// TestCreditProjectorRetriesFailedTerminalProjection verifies a transient
// activity-store failure does not memoize terminal state or clear pending
// tracking before the next poll durably records the transition.
func TestCreditProjectorRetriesFailedTerminalProjection(t *testing.T) {
	t.Parallel()

	reg := &fakeCreditRegistry{
		listResp: &credit.ListCreditOpsResponse{
			Ops: []credit.CreditOpSummary{{
				OpID:       "op-pay",
				OpKey:      "pay:" + payHashHex,
				Kind:       credit.KindPay,
				State:      credit.StateCompleted,
				CreditOnly: true,
				AmountSat:  500,
			}},
		},
	}
	store := &fakeActivityProjector{err: context.DeadlineExceeded}
	runtime := newRuntime(t.Context(), &Deps{
		CreditRegistry: reg,
		ActivityStore:  store,
	})
	t.Cleanup(runtime.stop)
	runtime.trackPendingEntryWithoutTimeout(&wavewalletrpc.WalletEntry{
		Id:     payHashHex,
		Kind:   wavewalletrpc.EntryKind_ENTRY_KIND_SEND,
		Status: wavewalletrpc.EntryStatus_ENTRY_STATUS_PENDING,
	})

	projected := make(map[string]credit.State)
	runtime.pollCreditOps(projected)
	require.NotContains(t, projected, "op-pay")
	require.Len(t, runtime.pendingSnapshot(), 1)
	require.Zero(t, store.count())

	store.err = nil
	runtime.pollCreditOps(projected)
	require.Equal(t, credit.StateCompleted, projected["op-pay"])
	require.Empty(t, runtime.pendingSnapshot())
	require.Equal(t, 1, store.count())
}
