//go:build wavewalletrpc && swapruntime

package swapwallet

import (
	"errors"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/lightninglabs/wavelength/rpc/swapclientrpc"
	"github.com/lightninglabs/wavelength/rpc/wavewalletrpc"
	"github.com/lightninglabs/wavelength/waverpc"
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
	resp *wavewalletrpc.PrepareSendResponse) (*wavewalletrpc.SendResponse,
	error) {

	t.Helper()

	return r.Send(t.Context(), &wavewalletrpc.SendRequest{
		SendIntentId: resp.GetSendIntentId(),
	})
}

func sendPreparedInvoice(t *testing.T, r *router, invoice string,
	maxFeeSat uint64) (*wavewalletrpc.SendResponse, error) {

	t.Helper()

	intent := &preparedSendIntent{
		kind:      preparedSendInvoice,
		invoice:   invoice,
		maxFeeSat: maxFeeSat,
	}
	id, err := r.intents.put(intent)
	require.NoError(t, err)

	return r.Send(t.Context(), &wavewalletrpc.SendRequest{
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
		t.Context(), &wavewalletrpc.PrepareSendRequest{
			Destination: &wavewalletrpc.PrepareSendRequest_Invoice{
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

// TestRouterPrepareSendInvoiceReturnsRemoteQuote confirms the invoice prepare
// path asks the swap client service for the non-mutating quote exposed by the
// remote swap server.
func TestRouterPrepareSendInvoiceReturnsRemoteQuote(t *testing.T) {
	t.Parallel()

	r, swap, rpc := newRouterFixture(t)
	invoice, paymentHash := testPreparedInvoice(t, 12_345, "coffee")
	swap.quotePayResp = &swapclientrpc.QuotePayResponse{
		PaymentHash:      paymentHash,
		InvoiceAmountSat: 12_345,
		AmountSat:        12_555,
		FeeSat:           210,
		SettlementType: swapclientrpc.
			SwapSettlementType_SWAP_SETTLEMENT_TYPE_LIGHTNING,
		ExpiresAtUnix: time.Now().Add(time.Minute).Unix(),
	}

	resp, err := r.PrepareSend(
		t.Context(), &wavewalletrpc.PrepareSendRequest{
			Destination: &wavewalletrpc.PrepareSendRequest_Invoice{
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
		t, wavewalletrpc.SendRail_SEND_RAIL_LIGHTNING, resp.GetRail(),
	)
	require.Equal(
		t, wavewalletrpc.SendQuoteStatus_SEND_QUOTE_STATUS_COMPLETE,
		resp.GetQuoteStatus(),
	)
	require.True(t, resp.GetFeeKnown())
	require.True(t, resp.GetTotalOutflowKnown())
	require.Equal(t, int64(210), resp.GetExpectedFeeSat())
	require.Equal(t, int64(12_555), resp.GetExpectedTotalOutflowSat())
	require.Equal(t, "coffee", resp.GetInvoiceDescription())
	require.Equal(t, paymentHash, resp.GetPaymentHash())
	require.Contains(t, resp.GetDestinationSummary(), "lnbcrt")
	require.Empty(t, resp.GetWarning())
	require.Equal(t, 1, swap.quotePayCalls)
	require.Equal(t, invoice, swap.quotePayLastReq.GetInvoice())
	require.Equal(t, uint64(25), swap.quotePayLastReq.GetMaxFeeSat())
	require.Equal(t, 0, swap.startPayCalls)
	require.Equal(t, 0, rpc.leaveCalls)
}

// TestRouterPrepareSendInvoiceFallsBackWhenQuoteUnavailable confirms mixed
// deployments can still prepare invoice sends when the quote RPC is missing.
func TestRouterPrepareSendInvoiceFallsBackWhenQuoteUnavailable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		code codes.Code
	}{{
		name: "grpc-unimplemented",
		code: codes.Unimplemented,
	}, {
		name: "rest-not-found",
		code: codes.NotFound,
	}}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			r, swap, rpc := newRouterFixture(t)
			invoice, paymentHash := testPreparedInvoice(
				t, 12_345, "mixed version",
			)
			swap.quotePayErr = status.Error(
				test.code, "missing quote",
			)

			resp, err := r.PrepareSend(
				t.Context(), &wavewalletrpc.PrepareSendRequest{
					Destination: &wavewalletrpc.
						PrepareSendRequest_Invoice{
						Invoice: invoice,
					},
					MaxFeeSat: 25,
				},
			)
			require.NoError(t, err)
			require.NotEmpty(t, resp.GetSendIntentId())
			require.Equal(t, int64(12_345), resp.GetAmountSat())
			require.Equal(
				t, wavewalletrpc.
					SendRail_SEND_RAIL_OFFCHAIN_UNKNOWN,
				resp.GetRail(),
			)
			require.Equal(
				t, wavewalletrpc.
					SendQuoteStatus_SEND_QUOTE_STATUS_LOCAL_ONLY,
				resp.GetQuoteStatus(),
			)
			require.False(t, resp.GetFeeKnown())
			require.False(t, resp.GetTotalOutflowKnown())
			require.Equal(t, int64(0), resp.GetExpectedFeeSat())
			require.Equal(
				t, int64(12_345),
				resp.GetExpectedTotalOutflowSat(),
			)
			require.Equal(
				t, "mixed version",
				resp.GetInvoiceDescription(),
			)
			require.Equal(t, paymentHash, resp.GetPaymentHash())
			require.Contains(
				t, resp.GetWarning(),
				"server quote unavailable",
			)
			require.Equal(t, 1, swap.quotePayCalls)
			require.Equal(
				t, invoice, swap.quotePayLastReq.GetInvoice(),
			)
			require.Equal(
				t, uint64(25),
				swap.quotePayLastReq.GetMaxFeeSat(),
			)
			require.Equal(t, 0, swap.startPayCalls)
			require.Equal(t, 0, rpc.leaveCalls)
		})
	}
}

// TestRouterPrepareSendInvoiceMapsUnknownQuoteRail confirms future quote rails
// still render the quoted amounts even before the wallet knows their names.
func TestRouterPrepareSendInvoiceMapsUnknownQuoteRail(t *testing.T) {
	t.Parallel()

	r, swap, _ := newRouterFixture(t)
	invoice, paymentHash := testPreparedInvoice(t, 12_345, "future rail")
	swap.quotePayResp = &swapclientrpc.QuotePayResponse{
		PaymentHash:      paymentHash,
		InvoiceAmountSat: 12_345,
		AmountSat:        12_555,
		FeeSat:           210,
		SettlementType: swapclientrpc.
			SwapSettlementType_SWAP_SETTLEMENT_TYPE_UNSPECIFIED,
		ExpiresAtUnix: time.Now().Add(time.Minute).Unix(),
	}

	resp, err := r.PrepareSend(
		t.Context(), &wavewalletrpc.PrepareSendRequest{
			Destination: &wavewalletrpc.PrepareSendRequest_Invoice{
				Invoice: invoice,
			},
		},
	)
	require.NoError(t, err)
	require.Equal(
		t, wavewalletrpc.SendRail_SEND_RAIL_OFFCHAIN_UNKNOWN,
		resp.GetRail(),
	)
	require.Equal(
		t, wavewalletrpc.SendQuoteStatus_SEND_QUOTE_STATUS_COMPLETE,
		resp.GetQuoteStatus(),
	)
	require.True(t, resp.GetFeeKnown())
	require.Equal(t, int64(210), resp.GetExpectedFeeSat())
	require.Equal(t, int64(12_555), resp.GetExpectedTotalOutflowSat())
}

// TestRouterPrepareSendInvoiceQuotesInArk confirms same-Ark previews surface
// as a concrete in-Ark rail with zero fee.
func TestRouterPrepareSendInvoiceQuotesInArk(t *testing.T) {
	t.Parallel()

	r, swap, rpc := newRouterFixture(t)
	invoice, paymentHash := testPreparedInvoice(t, 12_345, "ark")
	swap.quotePayResp = &swapclientrpc.QuotePayResponse{
		PaymentHash:      paymentHash,
		InvoiceAmountSat: 12_345,
		AmountSat:        12_345,
		FeeSat:           0,
		SettlementType: swapclientrpc.
			SwapSettlementType_SWAP_SETTLEMENT_TYPE_IN_ARK,
		ExpiresAtUnix: time.Now().Add(time.Minute).Unix(),
	}

	resp, err := r.PrepareSend(
		t.Context(), &wavewalletrpc.PrepareSendRequest{
			Destination: &wavewalletrpc.PrepareSendRequest_Invoice{
				Invoice: invoice,
			},
		},
	)
	require.NoError(t, err)
	require.Equal(
		t, wavewalletrpc.SendRail_SEND_RAIL_IN_ARK, resp.GetRail(),
	)
	require.Equal(t, int64(0), resp.GetExpectedFeeSat())
	require.Equal(t, int64(12_345),
		resp.GetExpectedTotalOutflowSat())
	require.Equal(t, 1, swap.quotePayCalls)
	require.Equal(t, 0, swap.startPayCalls)
	require.Equal(t, 0, rpc.leaveCalls)
}

// TestRouterPrepareSendInvoiceWarnsWhenQuoteExceedsMaxFee confirms PrepareSend
// keeps the non-mutating quote renderable when the caller's cap is too low.
func TestRouterPrepareSendInvoiceWarnsWhenQuoteExceedsMaxFee(t *testing.T) {
	t.Parallel()

	r, swap, _ := newRouterFixture(t)
	invoice, paymentHash := testPreparedInvoice(t, 12_345, "fee cap")
	swap.quotePayResp = &swapclientrpc.QuotePayResponse{
		PaymentHash:      paymentHash,
		InvoiceAmountSat: 12_345,
		AmountSat:        12_765,
		FeeSat:           420,
		SettlementType: swapclientrpc.
			SwapSettlementType_SWAP_SETTLEMENT_TYPE_LIGHTNING,
		ExceedsMaxFee: true,
		ExpiresAtUnix: time.Now().Add(time.Minute).Unix(),
	}

	resp, err := r.PrepareSend(
		t.Context(), &wavewalletrpc.PrepareSendRequest{
			Destination: &wavewalletrpc.PrepareSendRequest_Invoice{
				Invoice: invoice,
			},
			MaxFeeSat: 1,
		},
	)
	require.NoError(t, err)
	require.Equal(t, int64(420), resp.GetExpectedFeeSat())
	require.Contains(t, resp.GetWarning(), "exceeds max_fee_sat")
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
		t, wavewalletrpc.EntryKind_ENTRY_KIND_SEND,
		resp.GetEntry().GetKind(),
	)
}

// TestRouterSendInvoiceProjectsPendingEntry asserts that dispatching a
// pure-Lightning invoice eagerly projects the pending entry into the activity
// store, keyed by its payment hash, before Send returns. Without this the swap
// monitor's asynchronous SubscribeSwaps projection is the only writer, so a
// caller that polls InspectActivity to block on settlement (the default `da
// send` behavior) races the monitor and sees NOT_FOUND on the first poll,
// aborting the wait and reporting a still-in-flight pay as merely pending.
func TestRouterSendInvoiceProjectsPendingEntry(t *testing.T) {
	t.Parallel()

	r, swap, _ := newRouterFixture(t)
	store := &fakeActivityProjector{}
	r.deps.ActivityStore = store
	swap.startPayResp = &swapclientrpc.StartPayResponse{
		PaymentHash: "deadbeef",
		Swap: &swapclientrpc.SwapSummary{
			PaymentHash: "deadbeef",
			Direction: swapclientrpc.
				SwapDirection_SWAP_DIRECTION_PAY,
			Pending: true,
		},
	}

	resp, err := sendPreparedInvoice(t, r, "lnbc1example", 0)
	require.NoError(t, err)
	require.Equal(t, "deadbeef", resp.GetEntry().GetId())
	require.Equal(
		t, wavewalletrpc.EntryStatus_ENTRY_STATUS_PENDING,
		resp.GetEntry().GetStatus(),
	)

	// The dispatched pay must be durable and inspectable the instant Send
	// returns, so the wait loop can poll it through to a terminal state.
	require.Equal(t, 1, store.count())
	require.True(
		t, store.ids()["deadbeef"],
		"pending pay must be projected by payment hash on dispatch",
	)
	require.Equal(
		t, int64(wavewalletrpc.EntryStatus_ENTRY_STATUS_PENDING),
		store.lastProjection().Status,
		"eagerly projected row must be PENDING",
	)
}

// TestRouterSendInvoiceHandsCreditPayToRegistry asserts that a credit-backed
// pay is handed off to the durable credit registry under a payment-hash-derived
// idempotency key, rather than driving the top-up and pay inline. The router no
// longer calls CreateCredit/SendOOR/StartPay directly for credit pays.
func TestRouterSendInvoiceHandsCreditPayToRegistry(t *testing.T) {
	t.Parallel()

	r, swap, rpc := newRouterFixture(t)
	reg := &fakeCreditRegistry{}
	r.deps.CreditRegistry = reg

	invoice, paymentHash := testPreparedInvoice(t, 500, "tiny")
	swap.quotePayResp = &swapclientrpc.QuotePayResponse{
		PaymentHash:      paymentHash,
		InvoiceAmountSat: 500,
		AmountSat:        0,
		SettlementType: swapclientrpc.
			SwapSettlementType_SWAP_SETTLEMENT_TYPE_CREDIT,
		CreditQuote: &swapclientrpc.CreditQuote{
			MustUseCredit:      true,
			CreditShortfallSat: 500_000,
			CreditTopupSat:     1_000,
		},
		ExpiresAtUnix: time.Now().Add(time.Minute).Unix(),
	}

	prepareResp, err := r.PrepareSend(
		t.Context(), &wavewalletrpc.PrepareSendRequest{
			Destination: &wavewalletrpc.PrepareSendRequest_Invoice{
				Invoice: invoice,
			},
		},
	)
	require.NoError(t, err)
	require.Equal(
		t, uint64(1_000),
		prepareResp.GetCreditPreview().GetCreditTopupSat(),
	)

	resp, err := sendPrepared(t, r, prepareResp)
	require.NoError(t, err)
	require.Equal(t, int64(500), resp.GetActualAmountSat())

	// The pay was handed to the registry with the stable payment-hash key
	// and the quote's top-up amount.
	require.Equal(t, 1, reg.payCalls)
	require.NotNil(t, reg.lastPay)
	require.Equal(t, "pay:"+paymentHash, reg.lastPay.OpKey)
	require.Equal(t, uint64(1_000), reg.lastPay.TopupSat)
	require.Equal(t, invoice, reg.lastPay.Invoice)
	require.Equal(t, uint64(500_000), reg.lastPay.MaxCreditSat)

	// The router no longer drives the top-up or pay inline.
	require.Zero(t, swap.createCreditCalls)
	require.Zero(t, rpc.sendOORCalls)
	require.Zero(t, swap.startPayCalls)

	// A pending SEND entry keyed by the payment hash is surfaced.
	require.Equal(t, paymentHash, resp.GetEntry().GetId())
	require.Equal(
		t, wavewalletrpc.EntryStatus_ENTRY_STATUS_PENDING,
		resp.GetEntry().GetStatus(),
	)

	// A SEND is an outflow, so the pending row carries a negative amount
	// (issue #829).
	require.Equal(t, int64(-500), resp.GetEntry().GetAmountSat())
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
	rpc.listVTXOsResp = &waverpc.ListVTXOsResponse{
		Vtxos: []*waverpc.VTXO{
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
	rpc.sendOnChainResp = &waverpc.SendOnChainResponse{
		ActualAmountSat: 10_000,
		SelectedOutpoints: []string{
			"tx1:0",
			"tx2:1",
		},
		Status: "submitted",
	}

	// The operator returns a dynamic fee quote, so the preview is a
	// COMPLETE quote: net outflow is the amount delivered plus the fee,
	// not the gross VTXO bundle selected to cover it.
	rpc.estimateFeeResp = &waverpc.EstimateFeeResponse{
		TotalFeeSat: 500,
	}

	prepareResp, err := r.PrepareSend(
		t.Context(), &wavewalletrpc.PrepareSendRequest{
			Destination: &wavewalletrpc.
				PrepareSendRequest_OnchainAddress{
				OnchainAddress: "bcrt1qaddr",
			},
			AmtSat: 10000,
		},
	)
	require.NoError(t, err)
	require.Equal(
		t, wavewalletrpc.SendRail_SEND_RAIL_ONCHAIN,
		prepareResp.GetRail(),
	)
	require.Equal(
		t, wavewalletrpc.SendQuoteStatus_SEND_QUOTE_STATUS_COMPLETE,
		prepareResp.GetQuoteStatus(),
	)
	require.True(t, prepareResp.GetFeeKnown())
	require.Equal(
		t, int64(500), prepareResp.GetExpectedFeeSat(),
	)

	// Net outflow is amount + fee (10000 + 500); the ~2000 sat residual
	// over the 12000 sat selected bundle returns as a change VTXO and is
	// NOT part of the outflow.
	require.Equal(
		t, int64(10_500), prepareResp.GetExpectedTotalOutflowSat(),
	)

	// The preview selects largest-first (7000, then 5000), mirroring the
	// daemon's selection rather than the daemon's response order.
	require.Equal(
		t, []string{
			"tx2:1",
			"tx1:0",
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
		t, wavewalletrpc.EntryKind_ENTRY_KIND_EXIT,
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

	// Regression: wavelength#577. The prepare-time preview must
	// still filter to live VTXOs only so a stuck Forfeiting VTXO
	// from a prior leave doesn't inflate the preview's reported
	// outflow.
	require.Equal(
		t, waverpc.VTXOStatus_VTXO_STATUS_LIVE,
		rpc.listVTXOsLastReq.GetStatusFilter(),
		"prepare must filter to live VTXOs only (wavelength#577)",
	)

	_, err = sendPrepared(t, r, prepareResp)
	require.ErrorIs(t, err, ErrInvalidSendIntent)
	require.Equal(
		t, 1, rpc.sendOnChainCalls,
		"prepared send intents must be consume-once",
	)
}

// TestRouterSendOnchainFeeFallsBackToLocalFloor verifies that when the
// operator's EstimateFee quote is unavailable, the preview falls back to a
// local batch-size-1 fee floor derived from the daemon's cached operator
// terms, marks the quote LOCAL_ONLY, and surfaces a warning.
func TestRouterSendOnchainFeeFallsBackToLocalFloor(t *testing.T) {
	t.Parallel()

	r, _, rpc := newRouterFixture(t)
	rpc.listVTXOsResp = &waverpc.ListVTXOsResponse{
		Vtxos: []*waverpc.VTXO{
			{
				Outpoint:  "tx1:0",
				AmountSat: 10000,
			},
		},
	}

	// Operator quote unavailable forces the local floor.
	rpc.estimateFeeErr = errors.New("operator unreachable")

	// Cached operator terms drive the floor: at 10 sat/vByte a bounded
	// leave with one input and two outputs is
	// 60 + 58 + 2*43 = 204 vBytes, i.e. 2040 sats on-chain, which
	// exceeds the 300 sat minimum operator fee.
	rpc.getInfoResp = &waverpc.GetInfoResponse{
		WalletState: waverpc.WalletState_WALLET_STATE_READY,
		ServerInfo: &waverpc.ServerInfo{
			FeeRate:        10,
			MinOperatorFee: 300,
		},
	}

	prepareResp, err := r.PrepareSend(
		t.Context(), &wavewalletrpc.PrepareSendRequest{
			Destination: &wavewalletrpc.
				PrepareSendRequest_OnchainAddress{
				OnchainAddress: "bcrt1qaddr",
			},
			AmtSat: 5000,
		},
	)
	require.NoError(t, err)
	require.Equal(
		t, wavewalletrpc.SendQuoteStatus_SEND_QUOTE_STATUS_LOCAL_ONLY,
		prepareResp.GetQuoteStatus(),
	)
	require.False(t, prepareResp.GetFeeKnown())
	require.Equal(t, int64(2040), prepareResp.GetExpectedFeeSat())
	require.Equal(
		t, int64(7040), prepareResp.GetExpectedTotalOutflowSat(),
	)
	require.NotEmpty(t, prepareResp.GetWarning())
}

// TestRouterSendOnchainRejectsWhenFundsCannotCoverFee verifies that a
// bounded preview is rejected when the selected live VTXOs cover the
// principal but not the principal plus the quoted fee, rather than
// confirming an unaffordable outflow the real send would later fail.
func TestRouterSendOnchainRejectsWhenFundsCannotCoverFee(t *testing.T) {
	t.Parallel()

	r, _, rpc := newRouterFixture(t)

	// A single 6000 sat VTXO covers the 5000 sat principal (plus the
	// 300 sat selection headroom) but not the 5000 + 2040 sat amount +
	// fee, so the preview must reject rather than report a 7040 sat
	// outflow it cannot fund.
	rpc.listVTXOsResp = &waverpc.ListVTXOsResponse{
		Vtxos: []*waverpc.VTXO{
			{
				Outpoint:  "tx1:0",
				AmountSat: 6000,
			},
		},
	}
	rpc.estimateFeeErr = errors.New("operator unreachable")
	rpc.getInfoResp = &waverpc.GetInfoResponse{
		WalletState: waverpc.WalletState_WALLET_STATE_READY,
		ServerInfo: &waverpc.ServerInfo{
			FeeRate:        10,
			MinOperatorFee: 300,
		},
	}

	_, err := r.PrepareSend(
		t.Context(), &wavewalletrpc.PrepareSendRequest{
			Destination: &wavewalletrpc.
				PrepareSendRequest_OnchainAddress{
				OnchainAddress: "bcrt1qaddr",
			},
			AmtSat: 5000,
		},
	)
	require.ErrorIs(t, err, ErrAmountRequired)
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
	rpc.listVTXOsResp = &waverpc.ListVTXOsResponse{
		Vtxos: []*waverpc.VTXO{
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
	rpc.sendOnChainResp = &waverpc.SendOnChainResponse{
		ActualAmountSat: 12_000,
		SelectedOutpoints: []string{
			"tx1:0",
			"tx2:1",
		},
		Status: "submitted",
	}

	prepareResp, err := r.PrepareSend(
		t.Context(), &wavewalletrpc.PrepareSendRequest{
			Destination: &wavewalletrpc.
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

	// Regression: wavelength#577. Sweep-all prepare must also
	// filter to VTXO_STATUS_LIVE so a stuck Forfeiting VTXO from a
	// prior leave doesn't inflate the reported sweep total.
	require.Equal(
		t, waverpc.VTXOStatus_VTXO_STATUS_LIVE,
		rpc.listVTXOsLastReq.GetStatusFilter(),
		"sweep-all prepare must filter to live VTXOs only "+
			"(wavelength#577)",
	)
}

// TestRouterSendOnchainDaemonErrorBubblesUp asserts that when the
// daemon's SendOnChain RPC fails (insufficient funds, round actor
// rejection, selection shortfall, etc.) the error is surfaced to the
// caller wrapped with the router's context.
func TestRouterSendOnchainDaemonErrorBubblesUp(t *testing.T) {
	t.Parallel()

	r, _, rpc := newRouterFixture(t)
	rpc.listVTXOsResp = &waverpc.ListVTXOsResponse{
		Vtxos: []*waverpc.VTXO{
			{
				Outpoint:  "tx1:0",
				AmountSat: 10_000,
			},
		},
	}
	rpc.sendOnChainErr = errors.New("insufficient live VTXOs")

	prepareResp, err := r.PrepareSend(
		t.Context(), &wavewalletrpc.PrepareSendRequest{
			Destination: &wavewalletrpc.
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

	_, err := r.PrepareSend(t.Context(), &wavewalletrpc.PrepareSendRequest{
		Destination: &wavewalletrpc.PrepareSendRequest_OnchainAddress{
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

	_, err := r.PrepareSend(t.Context(), &wavewalletrpc.PrepareSendRequest{
		Destination: &wavewalletrpc.PrepareSendRequest_OnchainAddress{
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
	rpc.listVTXOsResp = &waverpc.ListVTXOsResponse{
		Vtxos: []*waverpc.VTXO{
			{
				Outpoint:  "tx1:0",
				AmountSat: 100,
			},
		},
	}

	_, err := r.PrepareSend(t.Context(), &wavewalletrpc.PrepareSendRequest{
		Destination: &wavewalletrpc.PrepareSendRequest_OnchainAddress{
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
		t.Context(), &wavewalletrpc.PrepareSendRequest{},
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
		t, wavewalletrpc.EntryKind_ENTRY_KIND_SEND,
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
