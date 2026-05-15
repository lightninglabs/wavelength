//go:build walletrpc && swapruntime

package swapwallet

import (
	"errors"
	"testing"

	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/lightninglabs/darepo-client/rpc/swapclientrpc"
	"github.com/lightninglabs/darepo-client/rpc/walletrpc"
	"github.com/stretchr/testify/require"
)

// newRouterFixture wires a router with the given fake deps, returning the
// router and the underlying fakes so tests can assert call counts.
func newRouterFixture(t *testing.T) (*router, *fakeSwapService,
	*fakeRPCServer) {

	t.Helper()

	swap := &fakeSwapService{}
	rpc := &fakeRPCServer{}
	deps := &Deps{
		SwapBackend: nil, // not used by router paths
		SwapService: swap,
		RPCServer:   rpc,
	}
	runtime := newRuntime(t.Context(), deps)
	t.Cleanup(runtime.stop)

	return newRouter(deps, runtime), swap, rpc
}

// TestRouterSendInvoiceDispatchesStartPay confirms an invoice destination
// routes through StartPay and never touches LeaveVTXOs.
func TestRouterSendInvoiceDispatchesStartPay(t *testing.T) {
	t.Parallel()

	r, swap, rpc := newRouterFixture(t)
	swap.startPayResp = &swapclientrpc.StartPayResponse{
		PaymentHash: "deadbeef",
		Swap: &swapclientrpc.SwapSummary{
			PaymentHash: "deadbeef",
			Direction: swapclientrpc.
				SwapDirection_SWAP_DIRECTION_PAY,
			Pending: true,
		},
	}

	resp, err := r.Send(t.Context(), &walletrpc.SendRequest{
		Destination: &walletrpc.SendRequest_Invoice{
			Invoice: "lnbc1example",
		},
		MaxFeeSat: 25,
	})
	require.NoError(t, err)
	require.Equal(t, 1, swap.startPayCalls)
	require.Equal(t, 0, rpc.leaveCalls)
	require.Equal(t, "lnbc1example", swap.startPayLastReq.GetInvoice())
	require.Equal(t, uint64(25), swap.startPayLastReq.GetMaxFeeSat())
	require.Equal(t, "deadbeef", resp.GetEntry().GetId())
	require.Equal(t,
		walletrpc.EntryKind_ENTRY_KIND_SEND, resp.GetEntry().GetKind(),
	)
}

// TestRouterSendOnchainSelectsVTXOsAndCallsLeave confirms that an onchain
// destination triggers VTXO selection via ListVTXOs and then a LeaveVTXOs
// call with the selected outpoints.
func TestRouterSendOnchainSelectsVTXOsAndCallsLeave(t *testing.T) {
	t.Parallel()

	r, swap, rpc := newRouterFixture(t)
	rpc.listVTXOsResp = &daemonrpc.ListVTXOsResponse{
		Vtxos: []*daemonrpc.VTXO{
			{Outpoint: "tx1:0", AmountSat: 5000},
			{Outpoint: "tx2:1", AmountSat: 7000},
			{Outpoint: "tx3:0", AmountSat: 3000},
		},
	}
	rpc.leaveResp = &daemonrpc.LeaveVTXOsResponse{
		QueuedOutpoints: []string{"tx1:0", "tx2:1"},
		Status:          "queued",
	}

	resp, err := r.Send(t.Context(), &walletrpc.SendRequest{
		Destination: &walletrpc.SendRequest_OnchainAddress{
			OnchainAddress: "bcrt1qaddr",
		},
		AmtSat: 10000,
	})
	require.NoError(t, err)
	require.Equal(t, 0, swap.startPayCalls)
	require.Equal(t, 1, rpc.leaveCalls)
	require.Equal(t, 1, rpc.listVTXOsCalls)

	got := rpc.leaveLastReq.GetOutpoints().GetOutpoints()
	require.Equal(t, []string{"tx1:0", "tx2:1"}, got,
		"selected outpoints must cover the target amount and stop "+
			"as soon as covered")
	require.Equal(t, "bcrt1qaddr",
		rpc.leaveLastReq.GetDefaultDestination().GetAddress(),
	)
	require.Equal(t,
		walletrpc.EntryKind_ENTRY_KIND_EXIT, resp.GetEntry().GetKind(),
	)
	require.Equal(t, "tx1:0", resp.GetEntry().GetId(),
		"the EXIT entry id is the first queued outpoint")
}

// TestRouterSendOnchainAmtZeroSweepsAll confirms that amt=0 routes through
// the LeaveVTXOs Selection.All path without touching ListVTXOs.
func TestRouterSendOnchainAmtZeroSweepsAll(t *testing.T) {
	t.Parallel()

	r, _, rpc := newRouterFixture(t)
	rpc.leaveResp = &daemonrpc.LeaveVTXOsResponse{
		QueuedOutpoints: []string{"tx1:0", "tx2:1"},
		Status:          "queued",
	}

	_, err := r.Send(t.Context(), &walletrpc.SendRequest{
		Destination: &walletrpc.SendRequest_OnchainAddress{
			OnchainAddress: "bcrt1qaddr",
		},
		AmtSat: 0,
	})
	require.NoError(t, err)
	require.Equal(t, 0, rpc.listVTXOsCalls,
		"amt=0 must not pre-select VTXOs")
	require.True(t, rpc.leaveLastReq.GetAll(),
		"amt=0 must trigger Selection.All")
}

// TestRouterSendOnchainInsufficientFunds confirms a request larger than
// the live VTXO sum returns ErrAmountRequired and never invokes LeaveVTXOs.
func TestRouterSendOnchainInsufficientFunds(t *testing.T) {
	t.Parallel()

	r, _, rpc := newRouterFixture(t)
	rpc.listVTXOsResp = &daemonrpc.ListVTXOsResponse{
		Vtxos: []*daemonrpc.VTXO{
			{Outpoint: "tx1:0", AmountSat: 100},
		},
	}

	_, err := r.Send(t.Context(), &walletrpc.SendRequest{
		Destination: &walletrpc.SendRequest_OnchainAddress{
			OnchainAddress: "bcrt1qaddr",
		},
		AmtSat: 10_000,
	})
	require.ErrorIs(t, err, ErrAmountRequired)
	require.Equal(t, 0, rpc.leaveCalls,
		"insufficient funds must not call LeaveVTXOs")
}

// TestRouterSendUnsetDestinationRejected asserts both invoice and onchain
// being unset returns ErrInvalidDestination cleanly.
func TestRouterSendUnsetDestinationRejected(t *testing.T) {
	t.Parallel()

	r, _, _ := newRouterFixture(t)

	_, err := r.Send(t.Context(), &walletrpc.SendRequest{})
	require.ErrorIs(t, err, ErrInvalidDestination)
}

// TestRouterSendInvoiceErrorBubblesUp asserts a StartPay error reaches
// the caller with the original error wrapped.
func TestRouterSendInvoiceErrorBubblesUp(t *testing.T) {
	t.Parallel()

	r, swap, _ := newRouterFixture(t)
	swap.startPayErr = errors.New("swap server unavailable")

	_, err := r.Send(t.Context(), &walletrpc.SendRequest{
		Destination: &walletrpc.SendRequest_Invoice{
			Invoice: "lnbc1example",
		},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "swap server unavailable")
}
