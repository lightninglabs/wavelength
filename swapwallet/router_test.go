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
	require.Equal(
		t, walletrpc.EntryKind_ENTRY_KIND_SEND,
		resp.GetEntry().GetKind(),
	)
}

// TestRouterSendOnchainSelectsVTXOsAndCallsLeave confirms that an onchain
// destination triggers VTXO selection via ListVTXOs and then a LeaveVTXOs
// call with the selected outpoints. It also asserts the response carries
// the actual amount that will leave the wallet (the sum of selected
// VTXOs), which under v1 whole-VTXO sweep semantics may exceed the
// caller's amt_sat.
func TestRouterSendOnchainSelectsVTXOsAndCallsLeave(t *testing.T) {
	t.Parallel()

	r, swap, rpc := newRouterFixture(t)
	rpc.listVTXOsResp = &daemonrpc.ListVTXOsResponse{
		Vtxos: []*daemonrpc.VTXO{
			{
				Outpoint:  "tx1:0",
				AmountSat: 5000,
			},
			{
				Outpoint:  "tx2:1",
				AmountSat: 7000,
			},
			{
				Outpoint:  "tx3:0",
				AmountSat: 3000,
			},
		},
	}
	rpc.leaveResp = &daemonrpc.LeaveVTXOsResponse{
		QueuedOutpoints: []string{
			"tx1:0",
			"tx2:1",
		},
		Status: "queued",
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
	require.Equal(
		t, []string{
			"tx1:0",
			"tx2:1",
		},
		got, "selected outpoints must cover the target amount and "+
			"stop as soon as covered",
	)
	require.Equal(
		t, "bcrt1qaddr",
		rpc.leaveLastReq.GetDefaultDestination().GetAddress(),
	)
	require.Equal(
		t, walletrpc.EntryKind_ENTRY_KIND_EXIT,
		resp.GetEntry().GetKind(),
	)
	require.Equal(
		t, "tx1:0", resp.GetEntry().GetId(),
		"the EXIT entry id is the first queued outpoint",
	)
	require.Equal(
		t, int64(12_000), resp.GetActualAmountSat(),
		"actual_amount_sat must be the SUM of selected VTXOs so "+
			"the caller can see whole-VTXO overpay before "+
			"treating the send as confirmed",
	)
}

// TestRouterSendOnchainSweepAllRoutesToAllSelection confirms that the
// explicit sweep_all flag routes through Selection.All and surfaces the
// total live VTXO sum on actual_amount_sat.
func TestRouterSendOnchainSweepAllRoutesToAllSelection(t *testing.T) {
	t.Parallel()

	r, _, rpc := newRouterFixture(t)
	rpc.listVTXOsResp = &daemonrpc.ListVTXOsResponse{
		Vtxos: []*daemonrpc.VTXO{
			{
				Outpoint:  "tx1:0",
				AmountSat: 5_000,
			},
			{
				Outpoint:  "tx2:1",
				AmountSat: 7_000,
			},
		},
	}
	rpc.leaveResp = &daemonrpc.LeaveVTXOsResponse{
		QueuedOutpoints: []string{
			"tx1:0",
			"tx2:1",
		},
		Status: "queued",
	}

	resp, err := r.Send(t.Context(), &walletrpc.SendRequest{
		Destination: &walletrpc.SendRequest_OnchainAddress{
			OnchainAddress: "bcrt1qaddr",
		},
		AmtSat:   0,
		SweepAll: true,
	})
	require.NoError(t, err)
	require.True(
		t, rpc.leaveLastReq.GetAll(),
		"sweep_all must trigger Selection.All",
	)
	require.Equal(
		t, int64(12_000), resp.GetActualAmountSat(),
		"actual_amount_sat on sweep must echo the total live VTXO sum",
	)
}

// TestRouterSendOnchainAmtZeroRejectedWithoutSweepAll asserts the
// commonest footgun — typo'd amt=0 — is rejected up front, structurally
// distinct from a deliberate wallet-draining sweep.
func TestRouterSendOnchainAmtZeroRejectedWithoutSweepAll(t *testing.T) {
	t.Parallel()

	r, _, rpc := newRouterFixture(t)

	_, err := r.Send(t.Context(), &walletrpc.SendRequest{
		Destination: &walletrpc.SendRequest_OnchainAddress{
			OnchainAddress: "bcrt1qaddr",
		},
		AmtSat: 0,
	})
	require.ErrorIs(t, err, ErrAmountRequired)
	require.Equal(
		t, 0, rpc.leaveCalls,
		"amt=0 with sweep_all=false must never reach LeaveVTXOs",
	)
	require.Equal(t, 0, rpc.listVTXOsCalls)
}

// TestRouterSendOnchainSweepAllRequiresZeroAmt asserts the contradictory
// combination amt>0 && sweep_all=true is rejected.
func TestRouterSendOnchainSweepAllRequiresZeroAmt(t *testing.T) {
	t.Parallel()

	r, _, rpc := newRouterFixture(t)

	_, err := r.Send(t.Context(), &walletrpc.SendRequest{
		Destination: &walletrpc.SendRequest_OnchainAddress{
			OnchainAddress: "bcrt1qaddr",
		},
		AmtSat:   1_000,
		SweepAll: true,
	})
	require.ErrorIs(t, err, ErrAmountInvalid)
	require.Equal(
		t, 0, rpc.leaveCalls,
		"sweep_all=true with amt>0 must never reach LeaveVTXOs",
	)
}

// TestRouterSendOnchainInsufficientFunds confirms a request larger than
// the live VTXO sum returns ErrAmountRequired and never invokes LeaveVTXOs.
func TestRouterSendOnchainInsufficientFunds(t *testing.T) {
	t.Parallel()

	r, _, rpc := newRouterFixture(t)
	rpc.listVTXOsResp = &daemonrpc.ListVTXOsResponse{
		Vtxos: []*daemonrpc.VTXO{
			{
				Outpoint:  "tx1:0",
				AmountSat: 100,
			},
		},
	}

	_, err := r.Send(t.Context(), &walletrpc.SendRequest{
		Destination: &walletrpc.SendRequest_OnchainAddress{
			OnchainAddress: "bcrt1qaddr",
		},
		AmtSat: 10_000,
	})
	require.ErrorIs(t, err, ErrAmountRequired)
	require.Equal(
		t, 0, rpc.leaveCalls,
		"insufficient funds must not call LeaveVTXOs",
	)
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
