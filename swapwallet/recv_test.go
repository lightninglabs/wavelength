//go:build walletdkrpc && swapruntime

package swapwallet

import (
	"errors"
	"testing"

	"github.com/lightninglabs/darepo-client/rpc/swapclientrpc"
	"github.com/lightninglabs/darepo-client/rpc/walletdkrpc"
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
