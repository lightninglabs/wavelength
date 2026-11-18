//go:build wavewalletrpc && swapruntime

package swapwallet

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/lightninglabs/wavelength/credit"
	"github.com/lightninglabs/wavelength/rpc/swapclientrpc"
	"github.com/lightninglabs/wavelength/rpc/wavewalletrpc"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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
// StartReceive with the caller's amount and returns the new invoice's phase.
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
			State: swapclientrpc.
				SwapState_SWAP_STATE_INVOICE_CREATED,
			Pending: true,
		},
	}

	resp, err := r.Recv(t.Context(), &wavewalletrpc.RecvRequest{
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
		t, wavewalletrpc.EntryKind_ENTRY_KIND_RECV,
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
		t, wavewalletrpc.
			WalletEntryPhase_WALLET_ENTRY_PHASE_WAITING_FOR_PAYMENT,
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

	resp, err := r.Recv(t.Context(), &wavewalletrpc.RecvRequest{
		AmtSat: 50_000,
		Memo:   "coffee",
	})
	require.NoError(t, err)
	require.Equal(t, "abc123", resp.GetEntry().GetId())
	require.Equal(
		t, wavewalletrpc.EntryStatus_ENTRY_STATUS_PENDING,
		resp.GetEntry().GetStatus(),
	)

	require.Equal(t, 1, store.count())
	require.True(
		t, store.ids()["abc123"],
		"pending recv must be projected by payment hash on dispatch",
	)
	require.Equal(
		t, int64(wavewalletrpc.EntryStatus_ENTRY_STATUS_PENDING),
		store.lastProjection().Status, "projected row must be PENDING",
	)
}

// TestRecvBelowVTXOFloorHandsToCreditRegistry asserts receive planning uses the
// larger operator VTXO minimum rather than routing an above-dust, sub-floor
// amount to a normal swap.
func TestRecvBelowVTXOFloorHandsToCreditRegistry(t *testing.T) {
	t.Parallel()
	const amountSat = 800

	swap := &fakeSwapService{}
	rpc := &fakeRPCServer{
		getInfoResp: &waverpc.GetInfoResponse{
			ServerInfo: &waverpc.ServerInfo{
				DustLimit:        546,
				MinVtxoAmountSat: 1_000,
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
		ActivityStore:  &fakeActivityProjector{},
	}
	runtime := newRuntime(t.Context(), deps)
	t.Cleanup(runtime.stop)
	receiver := newReceiver(deps, runtime)

	resp, err := receiver.Recv(
		t.Context(), &wavewalletrpc.RecvRequest{
			AmtSat: amountSat,
			Memo:   "tiny",
		},
	)
	require.NoError(t, err)

	// The receive was handed to the registry, not driven inline.
	require.Equal(t, 0, swap.startReceiveCalls)
	require.Equal(t, 0, swap.createCreditCalls)
	require.Equal(t, 1, reg.receiveCalls)
	require.NotNil(t, reg.lastReceive)
	require.Equal(t, uint64(amountSat), reg.lastReceive.AmountSat)
	require.Equal(t, "tiny", reg.lastReceive.Memo)
	require.Contains(t, reg.lastReceive.OpKey, "recv:")

	// The invoice and pending entry come from the registry response.
	require.Equal(t, "lnbc1credit", resp.GetInvoice())
	require.Equal(t, "cr_recv", resp.GetCreditReceive().GetOperationId())
	require.Equal(t, "abcd", resp.GetCreditReceive().GetPaymentHash())
	require.Equal(t, int64(amountSat), resp.GetEntry().GetAmountSat())
	require.Equal(
		t, wavewalletrpc.EntryKind_ENTRY_KIND_RECV,
		resp.GetEntry().GetKind(),
	)
	require.Equal(
		t, wavewalletrpc.
			WalletEntryPhase_WALLET_ENTRY_PHASE_WAITING_FOR_PAYMENT,
		resp.GetEntry().GetProgress().GetPhase(),
	)
	store := deps.ActivityStore.(*fakeActivityProjector)
	require.Equal(t, 1, store.count())
	projection := store.lastProjection()
	require.Equal(t, "cr_recv", projection.CanonicalID)
	require.Equal(t, int64(amountSat), projection.AmountSat)
	require.Equal(t, "tiny", projection.Note)
	require.NotEmpty(t, projection.RequestJSON)
	require.Equal(t, []byte{0xab, 0xcd}, projection.PaymentHash)
}

// TestRecvCreditFailureIsTypedAndRecorded locks the wavelength#1041 fix: when a
// sub-floor receive is routed to the credit subsystem and the swap server never
// completes the credit request (here a DeadlineExceeded, the exact symptom seen
// on signet), the failure must (1) surface as the typed
// ErrCreditReceiveUnavailable with actionable guidance rather than an opaque
// deadline error, (2) NOT leak the underlying DeadlineExceeded code through the
// error-mapping interceptor -- it must map to Unavailable, and (3) leave a
// FAILED activity row so the attempt is not silent.
func TestRecvCreditFailureIsTypedAndRecorded(t *testing.T) {
	t.Parallel()

	const (
		dustLimit = uint64(1_000)
		amt       = uint64(999)
	)

	swap := &fakeSwapService{}
	rpc := &fakeRPCServer{
		getInfoResp: &waverpc.GetInfoResponse{
			ServerInfo: &waverpc.ServerInfo{
				DustLimit: dustLimit,
			},
		},
	}
	reg := &fakeCreditRegistry{
		// Mirror the live signet symptom: the credit Ask resolves with
		// a gRPC DeadlineExceeded status (the credit admit timeout
		// firing).
		err: status.Error(
			codes.DeadlineExceeded, "context deadline exceeded",
		),
	}
	store := &fakeActivityProjector{}
	deps := &Deps{
		SwapService:    swap,
		RPCServer:      rpc,
		CreditRegistry: reg,
		ActivityStore:  store,
	}
	runtime := newRuntime(t.Context(), deps)
	t.Cleanup(runtime.stop)
	receiver := newReceiver(deps, runtime)

	_, err := receiver.Recv(t.Context(), &wavewalletrpc.RecvRequest{
		AmtSat: amt,
		Memo:   "tiny",
	})

	// (1) The failure is the typed sentinel carrying actionable guidance:
	// the requested amount and the VTXO floor to retry at or above.
	require.ErrorIs(t, err, ErrCreditReceiveUnavailable)
	require.Contains(t, err.Error(), "999")
	require.Contains(t, err.Error(), "1000")
	require.Equal(t, 1, reg.receiveCalls)
	require.Equal(t, 0, swap.startReceiveCalls)

	// (2) The returned error must NOT carry the underlying DeadlineExceeded
	// status: once mapped by the interceptor it is Unavailable, so a client
	// sees a distinct, retriable-elsewhere code rather than an opaque
	// deadline. This is the crux of the #1041 fix.
	mapped := mapSentinel(err)
	require.Equal(t, codes.Unavailable, status.Code(mapped))

	// (3) A FAILED activity row was projected so the attempt leaves a
	// trace, keyed to the credit counterparty and the requested amount.
	require.Equal(t, 1, store.count())
	row := store.lastProjection()
	require.Equal(
		t, int64(wavewalletrpc.EntryStatus_ENTRY_STATUS_FAILED),
		row.Status,
	)
	require.Equal(
		t, int64(wavewalletrpc.EntryKind_ENTRY_KIND_RECV), row.Kind,
	)
	require.Equal(t, "credit", row.Counterparty)
	require.Equal(t, int64(amt), row.AmountSat)
	require.NotEmpty(t, row.FailureReason)
}

// TestRecvCreditCallerContextIsPreserved asserts that caller cancellation is
// not reclassified as an operator credit outage or persisted as a failed
// receive attempt.
func TestRecvCreditCallerContextIsPreserved(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		want error
		ctx  func(*testing.T) context.Context
	}{
		{
			name: "canceled",
			want: context.Canceled,
			ctx: func(t *testing.T) context.Context {
				ctx, cancel := context.WithCancel(t.Context())
				cancel()

				return ctx
			},
		},
		{
			name: "deadline exceeded",
			want: context.DeadlineExceeded,
			ctx: func(t *testing.T) context.Context {
				ctx, cancel := context.WithDeadline(
					t.Context(),
					time.Now().Add(-time.Second),
				)
				t.Cleanup(cancel)

				return ctx
			},
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			registry := &fakeCreditRegistry{err: test.want}
			store := &fakeActivityProjector{}
			deps := &Deps{
				CreditRegistry: registry,
				ActivityStore:  store,
			}
			runtime := newRuntime(t.Context(), deps)
			t.Cleanup(runtime.stop)
			receiver := newReceiver(deps, runtime)

			_, err := receiver.recvCredit(
				test.ctx(t), &wavewalletrpc.RecvRequest{
					Memo: "tiny",
				}, 999, 1_000,
			)
			require.ErrorIs(t, err, test.want)
			require.Equal(t, 1, registry.receiveCalls)
			require.Equal(t, 0, store.count())
		})
	}
}

// TestRecvVTXOFloorLookupFailureFallsBackOpen asserts advisory floor planning
// does not block receives when the daemon has not fetched terms.
func TestRecvVTXOFloorLookupFailureFallsBackOpen(t *testing.T) {
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
		t.Context(), &wavewalletrpc.RecvRequest{
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

	_, err := r.Recv(t.Context(), &wavewalletrpc.RecvRequest{})
	require.ErrorIs(t, err, ErrAmountRequired)
	require.Equal(t, 0, swap.startReceiveCalls)
}

// TestRecvErrorBubblesUp confirms a StartReceive failure surfaces with
// the original error wrapped.
func TestRecvErrorBubblesUp(t *testing.T) {
	t.Parallel()

	r, swap := newRecvFixture(t)
	swap.startReceiveErr = errors.New("upstream broken")

	_, err := r.Recv(t.Context(), &wavewalletrpc.RecvRequest{
		AmtSat: 10_000,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "upstream broken")
}

// TestRecvRequestIDRecoversLostReply covers a keyed receive whose reply is
// lost: after a restart (a fresh receiver over the same durable swap store),
// LookupRecv returns the original invoice and entry, a repeated Recv returns
// the same result without creating a second invoice, and a changed request or
// an unknown ID is rejected.
func TestRecvRequestIDRecoversLostReply(t *testing.T) {
	t.Parallel()

	_, swap := newRecvFixture(t)
	swap.startReceiveResp = &swapclientrpc.StartReceiveResponse{
		PaymentHash: "abc123",
		Invoice:     "lnbc1invoice",
		Swap: &swapclientrpc.SwapSummary{
			PaymentHash: "abc123",
			Direction: swapclientrpc.
				SwapDirection_SWAP_DIRECTION_RECEIVE,
			State: swapclientrpc.
				SwapState_SWAP_STATE_INVOICE_CREATED,
			Pending: true,
		},
	}

	// newReceiver stands in for a daemon process: only the swap store,
	// modelled by the shared fake, survives between instances.
	newReceiverOverStore := func() *receiver {
		deps := &Deps{SwapService: swap}
		runtime := newRuntime(t.Context(), deps)
		t.Cleanup(runtime.stop)

		return newReceiver(deps, runtime)
	}
	req := &wavewalletrpc.RecvRequest{
		AmtSat: 50_000, Memo: "coffee", RequestId: "req-1",
	}

	// The first call is admitted but its reply never reaches the caller.
	_, err := newReceiverOverStore().Recv(t.Context(), req)
	require.NoError(t, err)
	require.Equal(t, "req-1", swap.startReceiveLast.GetIdempotencyKey())

	restarted := newReceiverOverStore()
	found, err := restarted.LookupRecv(
		t.Context(), &wavewalletrpc.LookupRecvRequest{
			RequestId: "req-1",
		},
	)
	require.NoError(t, err)
	require.Equal(t, "lnbc1invoice", found.GetInvoice())
	require.Equal(t, "abc123", found.GetEntry().GetId())
	require.Equal(t, "coffee", found.GetEntry().GetNote())
	require.Equal(t, 1, swap.startReceiveCalls)

	again, err := restarted.Recv(t.Context(), req)
	require.NoError(t, err)
	require.Equal(t, "lnbc1invoice", again.GetInvoice())
	require.Equal(t, "abc123", again.GetEntry().GetId())
	require.Equal(t, 1, swap.startReceiveCalls)

	_, err = restarted.Recv(t.Context(), &wavewalletrpc.RecvRequest{
		AmtSat: 60_000, Memo: "coffee", RequestId: "req-1",
	})
	require.ErrorIs(t, err, ErrReceiveRequestMismatch)
	require.Equal(t, 1, swap.startReceiveCalls)

	_, err = restarted.LookupRecv(
		t.Context(), &wavewalletrpc.LookupRecvRequest{
			RequestId: "unknown",
		},
	)
	require.ErrorIs(t, err, ErrReceiveNotFound)
	require.Equal(t, 1, swap.startReceiveCalls)
}

// TestRecvRequestIDCreditLookup verifies a keyed sub-floor receive uses the
// request ID as its credit op key, and that lookup returns the credit
// operation id that backs the receipt reference.
func TestRecvRequestIDCreditLookup(t *testing.T) {
	t.Parallel()

	reg := &fakeCreditRegistry{
		receiveResp: &credit.StartCreditResponse{
			OpID: "op-1", Invoice: "lnbc1credit",
			PaymentHash: []byte{
				0xab,
			},
		},
	}
	deps := &Deps{SwapService: &fakeSwapService{}, CreditRegistry: reg}
	runtime := newRuntime(t.Context(), deps)
	t.Cleanup(runtime.stop)
	r := newReceiver(deps, runtime)

	_, err := r.recvCredit(t.Context(), &wavewalletrpc.RecvRequest{
		AmtSat: 400, Memo: "tip", RequestId: "req-2",
	}, 400, 1_000)
	require.NoError(t, err)
	require.Equal(t, "recv:req-2", reg.lastReceive.OpKey)

	reg.lookupResp = &credit.CreditReceiveLookupResponse{
		OpID: "op-1", Invoice: "lnbc1credit",
		PaymentHash: []byte{
			0xab,
		}, AmountSat: 400, Memo: "tip",
		CreatedAt: time.Unix(1_000, 0),
	}
	found, err := r.LookupRecv(
		t.Context(), &wavewalletrpc.LookupRecvRequest{
			RequestId: "req-2",
		},
	)
	require.NoError(t, err)
	require.Equal(t, "lnbc1credit", found.GetInvoice())
	require.Equal(t, "op-1", found.GetCreditReceive().GetOperationId())
	require.Equal(t, "op-1", found.GetEntry().GetId())
	require.Equal(t, int64(1_000), found.GetEntry().GetCreatedAtUnix())
	require.Equal(t, 1, reg.receiveCalls)
}

// TestRecvRequestIDWaitsForInFlightCreate verifies a keyed receive still being
// created fences its ID: LookupRecv cannot answer NotFound while the create is
// in flight, and an overlapping Recv with the same ID returns the original
// instead of creating a second invoice.
func TestRecvRequestIDWaitsForInFlightCreate(t *testing.T) {
	t.Parallel()

	r, swap := newRecvFixture(t)
	swap.startReceiveResp = &swapclientrpc.StartReceiveResponse{
		PaymentHash: "abc123",
		Invoice:     "lnbc1invoice",
		Swap: &swapclientrpc.SwapSummary{
			PaymentHash: "abc123",
			Direction: swapclientrpc.
				SwapDirection_SWAP_DIRECTION_RECEIVE,
			State: swapclientrpc.
				SwapState_SWAP_STATE_INVOICE_CREATED,
			Pending: true,
		},
	}
	swap.startReceiveEntered = make(chan struct{})
	swap.startReceiveRelease = make(chan struct{})
	req := &wavewalletrpc.RecvRequest{
		AmtSat: 50_000, Memo: "coffee", RequestId: "req-1",
	}

	first := make(chan error, 1)
	go func() {
		_, err := r.Recv(t.Context(), req)
		first <- err
	}()
	<-swap.startReceiveEntered

	// The lookup must wait for the create, not report NotFound.
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	_, err := r.LookupRecv(ctx, &wavewalletrpc.LookupRecvRequest{
		RequestId: "req-1",
	})
	require.ErrorIs(t, err, context.DeadlineExceeded)

	second := make(chan *wavewalletrpc.RecvResponse, 1)
	go func() {
		resp, err := r.Recv(t.Context(), req)
		if err != nil {
			second <- nil

			return
		}
		second <- resp
	}()

	close(swap.startReceiveRelease)
	require.NoError(t, <-first)

	again := <-second
	require.NotNil(t, again)
	require.Equal(t, "lnbc1invoice", again.GetInvoice())
	require.Equal(t, 1, swap.startReceiveCalls)
}
