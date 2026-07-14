//go:build walletdkrpc && swapruntime

package swapwallet

import (
	"errors"
	"testing"

	"github.com/lightninglabs/wavelength/credit"
	"github.com/lightninglabs/wavelength/rpc/swapclientrpc"
	"github.com/lightninglabs/wavelength/rpc/walletdkrpc"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/stretchr/testify/require"
)

// newRecvFixture builds a receiver fixture using the same fake plumbing
// as the router tests so the two surfaces compose identically.
func newRecvFixture(t *testing.T) (*receiver, *fakeSwapService) {
	t.Helper()

	swap := &fakeSwapService{}
	deps := &Deps{
		SwapService: swap,
	}
	runtime := newRuntime(t.Context(), deps)
	t.Cleanup(runtime.stop)

	return newReceiver(deps, runtime), swap
}

// TestRecvDispatchesStartReceive confirms a valid Recv request reaches
// StartReceive with the caller's amount.
func TestRecvDispatchesStartReceive(t *testing.T) {
	t.Parallel()

	r, swap := newRecvFixture(t)
	swap.startReceiveResp = &swapclientrpc.StartReceiveResponse{
		PaymentHash: "abc123",
		Invoice:     "lnbc1invoice",
		Swap: &swapclientrpc.SwapSummary{
			PaymentHash: "abc123",
			Direction: swapclientrpc.
				SwapDirection_SWAP_DIRECTION_RECEIVE,
			Pending: true,
		},
	}

	resp, err := r.Recv(t.Context(), &walletdkrpc.RecvRequest{
		AmtSat: 50_000,
		Memo:   "coffee",
	})
	require.NoError(t, err)
	require.Equal(t, 1, swap.startReceiveCalls)
	require.Equal(t, "coffee", swap.startReceiveLast.GetMemo())
	require.Equal(t, "lnbc1invoice", resp.GetInvoice())
	require.Equal(t, "abc123", resp.GetEntry().GetId())
	require.Equal(t, "coffee", resp.GetEntry().GetNote())
	require.Equal(
		t, walletdkrpc.EntryKind_ENTRY_KIND_RECV,
		resp.GetEntry().GetKind(),
	)
	require.Equal(
		t, "lnbc1invoice",
		resp.GetEntry().GetRequest().GetLightningInvoice().GetInvoice(),
	)
	require.Equal(
		t, "abc123",
		resp.GetEntry().GetRequest().GetLightningInvoice().
			GetPaymentHash(),
	)
	require.Equal(
		t, walletdkrpc.WalletEntryPhase_WALLET_ENTRY_PHASE_SETTLING,
		resp.GetEntry().GetProgress().GetPhase(),
	)
}

// TestRecvProjectsPendingEntry confirms a swap-backed RECV projects its
// pending row on dispatch, keyed by payment hash, before Recv returns.
func TestRecvProjectsPendingEntry(t *testing.T) {
	t.Parallel()

	r, swap := newRecvFixture(t)
	store := &fakeActivityProjector{}
	r.deps.ActivityStore = store
	swap.startReceiveResp = &swapclientrpc.StartReceiveResponse{
		PaymentHash: "abc123",
		Invoice:     "lnbc1invoice",
		Swap: &swapclientrpc.SwapSummary{
			PaymentHash: "abc123",
			Direction: swapclientrpc.
				SwapDirection_SWAP_DIRECTION_RECEIVE,
			Pending: true,
		},
	}

	resp, err := r.Recv(t.Context(), &walletdkrpc.RecvRequest{
		AmtSat: 50_000,
		Memo:   "coffee",
	})
	require.NoError(t, err)
	require.Equal(t, "abc123", resp.GetEntry().GetId())
	require.Equal(
		t, walletdkrpc.EntryStatus_ENTRY_STATUS_PENDING,
		resp.GetEntry().GetStatus(),
	)

	require.Equal(t, 1, store.count())
	require.True(
		t, store.ids()["abc123"],
		"pending recv must be projected by payment hash on dispatch",
	)
	require.Equal(
		t, int64(walletdkrpc.EntryStatus_ENTRY_STATUS_PENDING),
		store.lastProjection().Status, "projected row must be PENDING",
	)
}

// TestRecvBelowDustHandsToCreditRegistry asserts a sub-dust receive is handed
// to the durable credit registry, which returns the server-owned invoice
// synchronously, instead of the wallet calling CreateCredit inline.
func TestRecvBelowDustHandsToCreditRegistry(t *testing.T) {
	t.Parallel()

	swap := &fakeSwapService{}
	rpc := &fakeRPCServer{
		getInfoResp: &waverpc.GetInfoResponse{
			ServerInfo: &waverpc.ServerInfo{
				DustLimit: 1_000,
			},
		},
	}
	reg := &fakeCreditRegistry{
		receiveResp: &credit.StartCreditResponse{
			OpID:    "cr_recv",
			Invoice: "lnbc1credit",
			PaymentHash: []byte{
				0xab,
				0xcd,
			},
		},
	}
	deps := &Deps{
		SwapService:    swap,
		RPCServer:      rpc,
		CreditRegistry: reg,
	}
	runtime := newRuntime(t.Context(), deps)
	t.Cleanup(runtime.stop)
	receiver := newReceiver(deps, runtime)

	resp, err := receiver.Recv(
		t.Context(), &walletdkrpc.RecvRequest{
			AmtSat: 500,
			Memo:   "tiny",
		},
	)
	require.NoError(t, err)

	// The receive was handed to the registry, not driven inline.
	require.Equal(t, 0, swap.startReceiveCalls)
	require.Equal(t, 0, swap.createCreditCalls)
	require.Equal(t, 1, reg.receiveCalls)
	require.NotNil(t, reg.lastReceive)
	require.Equal(t, uint64(500), reg.lastReceive.AmountSat)
	require.Equal(t, "tiny", reg.lastReceive.Memo)
	require.Contains(t, reg.lastReceive.OpKey, "recv:")

	// The invoice and pending entry come from the registry response.
	require.Equal(t, "lnbc1credit", resp.GetInvoice())
	require.Equal(t, "cr_recv", resp.GetCreditReceive().GetOperationId())
	require.Equal(t, "abcd", resp.GetCreditReceive().GetPaymentHash())
	require.Equal(t, int64(500), resp.GetEntry().GetAmountSat())
	require.Equal(
		t, walletdkrpc.EntryKind_ENTRY_KIND_RECV,
		resp.GetEntry().GetKind(),
	)
	require.Equal(
		t, walletdkrpc.
			WalletEntryPhase_WALLET_ENTRY_PHASE_WAITING_FOR_PAYMENT,
		resp.GetEntry().GetProgress().GetPhase(),
	)
}

// TestRecvDustLimitLookupFailureFallsBackOpen asserts that advisory dust
// planning does not block receives when the daemon has not fetched terms.
func TestRecvDustLimitLookupFailureFallsBackOpen(t *testing.T) {
	t.Parallel()

	swap := &fakeSwapService{
		startReceiveResp: &swapclientrpc.StartReceiveResponse{
			PaymentHash: "abc123",
			Invoice:     "lnbc1invoice",
			Swap: &swapclientrpc.SwapSummary{
				PaymentHash: "abc123",
				Direction: swapclientrpc.
					SwapDirection_SWAP_DIRECTION_RECEIVE,
				Pending: true,
			},
		},
	}
	rpc := &fakeRPCServer{
		getInfoErr: errors.New("terms unavailable"),
	}
	reg := &fakeCreditRegistry{
		receiveResp: &credit.StartCreditResponse{
			OpID:    "cr_recv",
			Invoice: "lnbc1credit",
		},
	}
	deps := &Deps{
		SwapService:    swap,
		RPCServer:      rpc,
		CreditRegistry: reg,
	}
	runtime := newRuntime(t.Context(), deps)
	t.Cleanup(runtime.stop)
	receiver := newReceiver(deps, runtime)

	resp, err := receiver.Recv(
		t.Context(), &walletdkrpc.RecvRequest{
			AmtSat: 500,
			Memo:   "tiny",
		},
	)
	require.NoError(t, err)

	require.Equal(t, 1, swap.startReceiveCalls)
	require.Equal(t, 0, reg.receiveCalls)
	require.Equal(t, "lnbc1invoice", resp.GetInvoice())
}

// TestRecvAmtZeroRejected asserts a missing amount returns
// ErrAmountRequired without invoking the swap service.
func TestRecvAmtZeroRejected(t *testing.T) {
	t.Parallel()

	r, swap := newRecvFixture(t)

	_, err := r.Recv(t.Context(), &walletdkrpc.RecvRequest{})
	require.ErrorIs(t, err, ErrAmountRequired)
	require.Equal(t, 0, swap.startReceiveCalls)
}

// TestRecvErrorBubblesUp confirms a StartReceive failure surfaces with
// the original error wrapped.
func TestRecvErrorBubblesUp(t *testing.T) {
	t.Parallel()

	r, swap := newRecvFixture(t)
	swap.startReceiveErr = errors.New("upstream broken")

	_, err := r.Recv(t.Context(), &walletdkrpc.RecvRequest{
		AmtSat: 10_000,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "upstream broken")
}
