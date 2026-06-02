//go:build walletdkrpc && swapruntime

package swapwallet

import (
	"errors"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/lightninglabs/darepo-client/rpc/swapclientrpc"
	"github.com/lightninglabs/darepo-client/rpc/walletdkrpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/zpay32"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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
		ChainParams: &chaincfg.RegressionNetParams,
	}
	runtime := newRuntime(t.Context(), deps)
	t.Cleanup(runtime.stop)

	return newRouter(deps, runtime), swap, rpc
}

func sendPrepared(t *testing.T, r *router,
	resp *walletdkrpc.PrepareSendResponse) (*walletdkrpc.SendResponse,
	error) {

	t.Helper()

	return r.Send(t.Context(), &walletdkrpc.SendRequest{
		SendIntentId: resp.GetSendIntentId(),
	})
}

func sendPreparedInvoice(t *testing.T, r *router, invoice string,
	maxFeeSat uint64) (*walletdkrpc.SendResponse, error) {

	t.Helper()

	intent := &preparedSendIntent{
		kind:      preparedSendInvoice,
		invoice:   invoice,
		maxFeeSat: maxFeeSat,
	}
	id, err := r.intents.put(intent)
	require.NoError(t, err)

	return r.Send(t.Context(), &walletdkrpc.SendRequest{
		SendIntentId: id,
	})
}

func testPreparedInvoice(t *testing.T, amountSat btcutil.Amount,
	description string) (string, string) {

	t.Helper()

	return testPreparedInvoiceOnNet(
		t, &chaincfg.RegressionNetParams, amountSat, description,
	)
}

func testPreparedInvoiceOnNet(t *testing.T, params *chaincfg.Params,
	amountSat btcutil.Amount, description string) (string, string) {

	t.Helper()

	invoiceKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	preimage := lntypes.Preimage{
		0x01, 0x02, 0x03, 0x04,
	}
	paymentHash := preimage.Hash()

	invoice, err := zpay32.NewInvoice(
		params, paymentHash, time.Now(),
		zpay32.Amount(
			lnwire.NewMSatFromSatoshis(amountSat),
		),
		zpay32.Description(description),
	)
	require.NoError(t, err)

	paymentRequest, err := invoice.Encode(zpay32.MessageSigner{
		SignCompact: func(msg []byte) ([]byte, error) {
			return ecdsa.SignCompact(invoiceKey, msg, true), nil
		},
	})
	require.NoError(t, err)

	return paymentRequest, paymentHash.String()
}

// TestRouterPrepareSendInvoiceRejectsWrongNetwork confirms prepare decodes
// BOLT-11 invoices against the daemon network before issuing an intent.
func TestRouterPrepareSendInvoiceRejectsWrongNetwork(t *testing.T) {
	t.Parallel()

	r, swap, rpc := newRouterFixture(t)
	invoice, _ := testPreparedInvoiceOnNet(
		t, &chaincfg.MainNetParams, 12_345, "wrong network",
	)

	_, err := r.PrepareSend(
		t.Context(), &walletdkrpc.PrepareSendRequest{
			Destination: &walletdkrpc.PrepareSendRequest_Invoice{
				Invoice: invoice,
			},
		},
	)
	require.ErrorIs(t, err, ErrInvalidDestination)
	require.Equal(t, 0, swap.startPayCalls)
	require.Equal(t, 0, rpc.leaveCalls)

	r.intents.mu.Lock()
	intentCount := len(r.intents.intents)
	r.intents.mu.Unlock()
	require.Zero(t, intentCount, "wrong-network invoices create no intent")
}

// TestRouterPrepareSendInvoiceReturnsLocalPreview confirms the invoice
// prepare path decodes the BOLT-11 metadata locally, produces a short-lived
// intent, and honestly marks fee/rail details as local-only until remote quote
// APIs exist.
func TestRouterPrepareSendInvoiceReturnsLocalPreview(t *testing.T) {
	t.Parallel()

	r, swap, rpc := newRouterFixture(t)
	invoice, paymentHash := testPreparedInvoice(t, 12_345, "coffee")

	resp, err := r.PrepareSend(
		t.Context(), &walletdkrpc.PrepareSendRequest{
			Destination: &walletdkrpc.PrepareSendRequest_Invoice{
				Invoice: invoice,
			},
			AmtSat:    99_999,
			MaxFeeSat: 25,
		},
	)
	require.NoError(t, err)
	require.NotEmpty(t, resp.GetSendIntentId())
	require.Equal(t, int64(12_345), resp.GetAmountSat())
	require.Equal(
		t, walletdkrpc.SendRail_SEND_RAIL_OFFCHAIN_UNKNOWN,
		resp.GetRail(),
	)
	require.Equal(
		t, walletdkrpc.SendQuoteStatus_SEND_QUOTE_STATUS_LOCAL_ONLY,
		resp.GetQuoteStatus(),
	)
	require.False(t, resp.GetFeeKnown())
	require.False(t, resp.GetTotalOutflowKnown())
	require.Equal(t, int64(0), resp.GetExpectedFeeSat())
	require.Equal(t, int64(0), resp.GetExpectedTotalOutflowSat())
	require.Equal(t, "coffee", resp.GetInvoiceDescription())
	require.Equal(t, paymentHash, resp.GetPaymentHash())
	require.Contains(t, resp.GetDestinationSummary(), "lnbcrt")
	require.Contains(t, resp.GetWarning(), "swapserver quote support")
	require.Equal(t, 0, swap.startPayCalls)
	require.Equal(t, 0, rpc.leaveCalls)
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

	resp, err := sendPreparedInvoice(t, r, "lnbc1example", 25)
	require.NoError(t, err)
	require.Equal(t, 1, swap.startPayCalls)
	require.Equal(t, 0, rpc.leaveCalls)
	require.Equal(t, "lnbc1example", swap.startPayLastReq.GetInvoice())
	require.Equal(t, uint64(25), swap.startPayLastReq.GetMaxFeeSat())
	require.Equal(t, "deadbeef", resp.GetEntry().GetId())
	require.Equal(
		t, walletdkrpc.EntryKind_ENTRY_KIND_SEND,
		resp.GetEntry().GetKind(),
	)
}

// TestRouterSendOnchainSelectsVTXOsAndCallsLeave confirms that an onchain
// destination triggers a local VTXO snapshot via ListVTXOs during prepare
// (for the preview), and that the Send step then dispatches to the
// daemon's SendOnChain RPC with the caller's --amt passed through
// exactly. ActualAmountSat on the response equals --amt: the exact-
// amount path (issue #634) replaces the previous whole-VTXO sweep where
// the destination could receive significantly more than --amt.
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
	rpc.sendOnChainResp = &daemonrpc.SendOnChainResponse{
		ActualAmountSat: 10_000,
		SelectedOutpoints: []string{
			"tx1:0",
			"tx2:1",
		},
		Status: "submitted",
	}

	prepareResp, err := r.PrepareSend(
		t.Context(), &walletdkrpc.PrepareSendRequest{
			Destination: &walletdkrpc.
				PrepareSendRequest_OnchainAddress{
				OnchainAddress: "bcrt1qaddr",
			},
			AmtSat: 10000,
		},
	)
	require.NoError(t, err)
	require.Equal(
		t, walletdkrpc.SendRail_SEND_RAIL_ONCHAIN,
		prepareResp.GetRail(),
	)
	require.Equal(
		t, walletdkrpc.SendQuoteStatus_SEND_QUOTE_STATUS_LOCAL_ONLY,
		prepareResp.GetQuoteStatus(),
	)
	require.Equal(
		t, int64(12_000), prepareResp.GetExpectedTotalOutflowSat(),
	)
	require.Equal(
		t, []string{
			"tx1:0",
			"tx2:1",
		},
		prepareResp.GetSelectedOutpoints(),
	)
	resp, err := sendPrepared(t, r, prepareResp)
	require.NoError(t, err)
	require.Equal(t, 0, swap.startPayCalls)
	require.Equal(
		t, 0, rpc.leaveCalls,
		"new SendOnChain path must not fall back to LeaveVTXOs",
	)
	require.Equal(t, 1, rpc.sendOnChainCalls)
	require.Equal(
		t, 1, rpc.listVTXOsCalls,
		"prepare-time preview still snapshots live VTXOs",
	)
	require.Equal(
		t, 0, rpc.joinNextRoundCalls, "SendOnChain registers "+
			"atomically; no JoinNextRound from the wallet layer",
	)

	gotReq := rpc.sendOnChainLastReq
	require.Equal(
		t, int64(10_000), gotReq.GetAmountSat(),
		"--amt is passed to SendOnChain exactly under the new "+
			"exact-amount semantics (issue #634)",
	)
	require.False(t, gotReq.GetSweepAll())
	require.Equal(
		t, "bcrt1qaddr",
		gotReq.GetDestination().GetAddress(),
	)
	require.Equal(
		t, walletdkrpc.EntryKind_ENTRY_KIND_EXIT,
		resp.GetEntry().GetKind(),
	)
	require.Equal(
		t, "tx1:0", resp.GetEntry().GetId(),
		"the EXIT entry id is the first selected outpoint",
	)
	require.Equal(
		t, int64(10_000), resp.GetActualAmountSat(),
		"actual_amount_sat must equal --amt exactly under the new "+
			"change-VTXO semantics (issue #634)",
	)

	// Regression: darepo-client#577. The prepare-time preview must
	// still filter to live VTXOs only so a stuck Forfeiting VTXO
	// from a prior leave doesn't inflate the preview's reported
	// outflow.
	require.Equal(
		t, daemonrpc.VTXOStatus_VTXO_STATUS_LIVE,
		rpc.listVTXOsLastReq.GetStatusFilter(),
		"prepare must filter to live VTXOs only (darepo-client#577)",
	)

	_, err = sendPrepared(t, r, prepareResp)
	require.ErrorIs(t, err, ErrInvalidSendIntent)
	require.Equal(
		t, 1, rpc.sendOnChainCalls,
		"prepared send intents must be consume-once",
	)
}

// TestRouterSendOnchainSweepAllRoutesToSendOnChain confirms that the
// explicit sweep_all flag snapshots the live set during prepare (for the
// preview total) and dispatches to SendOnChain in sweep_all mode at
// Send time. The daemon enumerates the live set again at execution
// time, so the actual swept set may diverge from the prepare-time
// snapshot if new VTXOs land between prepare and Send.
func TestRouterSendOnchainSweepAllRoutesToSendOnChain(t *testing.T) {
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
	rpc.sendOnChainResp = &daemonrpc.SendOnChainResponse{
		ActualAmountSat: 12_000,
		SelectedOutpoints: []string{
			"tx1:0",
			"tx2:1",
		},
		Status: "submitted",
	}

	prepareResp, err := r.PrepareSend(
		t.Context(), &walletdkrpc.PrepareSendRequest{
			Destination: &walletdkrpc.
				PrepareSendRequest_OnchainAddress{
				OnchainAddress: "bcrt1qaddr",
			},
			AmtSat:   0,
			SweepAll: true,
		},
	)
	require.NoError(t, err)
	require.Equal(t, int64(12_000), prepareResp.GetAmountSat())
	require.Equal(
		t, int64(12_000), prepareResp.GetExpectedTotalOutflowSat(),
	)
	resp, err := sendPrepared(t, r, prepareResp)
	require.NoError(t, err)
	require.Equal(t, 1, rpc.sendOnChainCalls)
	require.True(
		t, rpc.sendOnChainLastReq.GetSweepAll(),
		"sweep_all must set the sweep_all variant on SendOnChain",
	)
	require.Equal(
		t, int64(12_000), resp.GetActualAmountSat(),
		"actual_amount_sat on sweep must echo the daemon response",
	)

	// Regression: darepo-client#577. Sweep-all prepare must also
	// filter to VTXO_STATUS_LIVE so a stuck Forfeiting VTXO from a
	// prior leave doesn't inflate the reported sweep total.
	require.Equal(
		t, daemonrpc.VTXOStatus_VTXO_STATUS_LIVE,
		rpc.listVTXOsLastReq.GetStatusFilter(),
		"sweep-all prepare must filter to live VTXOs only "+
			"(darepo-client#577)",
	)
}

// TestRouterSendOnchainDaemonErrorBubblesUp asserts that when the
// daemon's SendOnChain RPC fails (insufficient funds, round actor
// rejection, selection shortfall, etc.) the error is surfaced to the
// caller wrapped with the router's context.
func TestRouterSendOnchainDaemonErrorBubblesUp(t *testing.T) {
	t.Parallel()

	r, _, rpc := newRouterFixture(t)
	rpc.listVTXOsResp = &daemonrpc.ListVTXOsResponse{
		Vtxos: []*daemonrpc.VTXO{
			{
				Outpoint:  "tx1:0",
				AmountSat: 10_000,
			},
		},
	}
	rpc.sendOnChainErr = errors.New("insufficient live VTXOs")

	prepareResp, err := r.PrepareSend(
		t.Context(), &walletdkrpc.PrepareSendRequest{
			Destination: &walletdkrpc.
				PrepareSendRequest_OnchainAddress{
				OnchainAddress: "bcrt1qaddr",
			},
			AmtSat: 5_000,
		},
	)
	require.NoError(t, err)
	_, err = sendPrepared(t, r, prepareResp)
	require.Error(t, err)
	require.ErrorContains(t, err, "send on-chain")
	require.ErrorContains(t, err, "insufficient live VTXOs")
	require.Equal(t, 1, rpc.sendOnChainCalls)
}

// TestRouterSendOnchainAmtZeroRejectedWithoutSweepAll asserts the
// commonest footgun — typo'd amt=0 — is rejected up front, structurally
// distinct from a deliberate wallet-draining sweep.
func TestRouterSendOnchainAmtZeroRejectedWithoutSweepAll(t *testing.T) {
	t.Parallel()

	r, _, rpc := newRouterFixture(t)

	_, err := r.PrepareSend(t.Context(), &walletdkrpc.PrepareSendRequest{
		Destination: &walletdkrpc.PrepareSendRequest_OnchainAddress{
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

	_, err := r.PrepareSend(t.Context(), &walletdkrpc.PrepareSendRequest{
		Destination: &walletdkrpc.PrepareSendRequest_OnchainAddress{
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

	_, err := r.PrepareSend(t.Context(), &walletdkrpc.PrepareSendRequest{
		Destination: &walletdkrpc.PrepareSendRequest_OnchainAddress{
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

// TestRouterPrepareSendUnsetDestinationRejected asserts both invoice and
// onchain being unset returns ErrInvalidDestination cleanly.
func TestRouterPrepareSendUnsetDestinationRejected(t *testing.T) {
	t.Parallel()

	r, _, _ := newRouterFixture(t)

	_, err := r.PrepareSend(
		t.Context(), &walletdkrpc.PrepareSendRequest{},
	)
	require.ErrorIs(t, err, ErrInvalidDestination)
}

// TestRouterPrepareSendNilRequestRejected confirms direct in-process callers
// get the same validation error as an empty request rather than a panic.
func TestRouterPrepareSendNilRequestRejected(t *testing.T) {
	t.Parallel()

	r, _, _ := newRouterFixture(t)

	_, err := r.PrepareSend(t.Context(), nil)
	require.ErrorIs(t, err, ErrInvalidDestination)
}

// TestRouterSendNilRequestRejected confirms a missing prepared intent id is
// rejected before the store path can dereference a nil request.
func TestRouterSendNilRequestRejected(t *testing.T) {
	t.Parallel()

	r, _, _ := newRouterFixture(t)

	_, err := r.Send(t.Context(), nil)
	require.ErrorIs(t, err, ErrInvalidSendIntent)
}

// TestRouterSendInvoiceAmountSignedFromCallerKind asserts that an
// initial StartPay summary returned with UNSPECIFIED direction (the SDK
// fills it in on the publish-hash update) still produces a SEND entry
// with a NEGATIVE amount, because the wallet layer pins the kind on
// submit. Prior to this fix the amount was sign-derived from
// s.GetDirection(), so UNSPECIFIED kept the amount positive and the
// CLI printed +N for an outgoing send.
func TestRouterSendInvoiceAmountSignedFromCallerKind(t *testing.T) {
	t.Parallel()

	r, swap, _ := newRouterFixture(t)
	swap.startPayResp = &swapclientrpc.StartPayResponse{
		PaymentHash: "deadbeef",
		Swap: &swapclientrpc.SwapSummary{
			PaymentHash: "deadbeef",
			Direction: swapclientrpc.
				SwapDirection_SWAP_DIRECTION_UNSPECIFIED,
			AmountSat: 10_000,
			Pending:   true,
		},
	}

	resp, err := sendPreparedInvoice(t, r, "lnbc1example", 0)
	require.NoError(t, err)
	require.Equal(
		t, walletdkrpc.EntryKind_ENTRY_KIND_SEND,
		resp.GetEntry().GetKind(),
	)
	require.Equal(
		t, int64(-10_000), resp.GetEntry().GetAmountSat(),
		"SEND amount must be negative even when the SDK summary "+
			"direction is UNSPECIFIED on the initial response",
	)
}

// TestRouterSendInvoiceErrorBubblesUp asserts a StartPay error reaches
// the caller with the original error wrapped.
func TestRouterSendInvoiceErrorBubblesUp(t *testing.T) {
	t.Parallel()

	r, swap, _ := newRouterFixture(t)
	swap.startPayErr = errors.New("swap server unavailable")

	_, err := sendPreparedInvoice(t, r, "lnbc1example", 0)
	require.Error(t, err)
	require.Contains(t, err.Error(), "swap server unavailable")
}

// TestRouterSendInvoicePreservesStartPayStatusCode verifies StartPay status
// errors keep their gRPC code as they move through the wallet router.
func TestRouterSendInvoicePreservesStartPayStatusCode(t *testing.T) {
	t.Parallel()

	r, swap, _ := newRouterFixture(t)
	startPayErr := status.Error(
		codes.AlreadyExists, "receive intent already used",
	)
	swap.startPayErr = startPayErr

	_, err := sendPreparedInvoice(t, r, "lnbc1example", 0)
	require.Error(t, err)
	require.Equal(t, codes.AlreadyExists, status.Code(err))
	require.ErrorIs(t, err, startPayErr)
	require.Contains(t, status.Convert(err).Message(), "start pay")
	require.Contains(
		t, status.Convert(err).Message(),
		"receive intent already used",
	)
}
