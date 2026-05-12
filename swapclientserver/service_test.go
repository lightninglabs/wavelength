//go:build swapruntime

package swapclientserver

import (
	"context"
	"encoding/hex"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/darepod"
	"github.com/lightninglabs/darepo-client/rpc/swapclientrpc"
	"github.com/lightninglabs/darepo-client/sdk/swaps"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
)

func TestResumePendingStartsWorkersAndDedupes(t *testing.T) {
	t.Parallel()

	payHash := testHash(1)
	receiveHash := testHash(2)
	fakeClient := newFakeSwapRuntime(
		swaps.SwapSummary{
			Direction:   swaps.SwapDirectionPay,
			PaymentHash: payHash,
			State:       "funding",
			Pending:     true,
		},
		swaps.SwapSummary{
			Direction:   swaps.SwapDirectionReceive,
			PaymentHash: receiveHash,
			State:       "invoice_created",
			Pending:     true,
		},
	)
	service := newTestSwapClientService(fakeClient)
	defer service.cancel()

	service.resumePending(t.Context())
	fakeClient.awaitPayResume(t, payHash)
	fakeClient.awaitReceiveResume(t, receiveHash)

	_, err := service.ResumeSwap(
		t.Context(), &swapclientrpc.ResumeSwapRequest{
			PaymentHash: hex.EncodeToString(payHash[:]),
			Direction: swapclientrpc.
				SwapDirection_SWAP_DIRECTION_PAY,
		},
	)
	require.NoError(t, err)

	require.Equal(t, 1, fakeClient.payResumeCount(payHash))
	require.Equal(t, 1, fakeClient.receiveResumeCount(receiveHash))
	require.True(t, fakeClient.sawPendingOnlyList())
}

func TestStartPayReturnsSummaryAndStartsWorker(t *testing.T) {
	t.Parallel()

	payHash := testHash(3)
	fakeClient := newFakeSwapRuntime(
		swaps.SwapSummary{
			Direction:   swaps.SwapDirectionPay,
			PaymentHash: payHash,
			State:       "created",
			Pending:     true,
			AmountSat:   10_000,
			MaxFeeSat:   25,
		},
	)
	fakeClient.startPaySession = &fakePaySession{hash: payHash}
	service := newTestSwapClientService(fakeClient)
	defer service.cancel()

	resp, err := service.StartPay(
		t.Context(), &swapclientrpc.StartPayRequest{
			Invoice:   "lnbc1test",
			MaxFeeSat: 25,
		},
	)
	require.NoError(t, err)
	require.Equal(t, hex.EncodeToString(payHash[:]), resp.GetPaymentHash())
	require.Equal(t, int64(10_000), resp.GetSwap().GetAmountSat())

	fakeClient.awaitPayResume(t, payHash)
	require.Equal(t, 1, fakeClient.startPayCount())
	require.Equal(t, 1, fakeClient.payResumeCount(payHash))
}

func TestStartReceiveReturnsInvoiceAndStartsWorker(t *testing.T) {
	t.Parallel()

	receiveHash := testHash(4)
	fakeClient := newFakeSwapRuntime(
		swaps.SwapSummary{
			Direction:   swaps.SwapDirectionReceive,
			PaymentHash: receiveHash,
			State:       "invoice_created",
			Pending:     true,
			AmountSat:   50_000,
		},
	)
	fakeClient.startReceiveSession = &fakeReceiveSession{
		hash:    receiveHash,
		invoice: "lnbc1receive",
	}
	service := newTestSwapClientService(fakeClient)
	defer service.cancel()

	resp, err := service.StartReceive(
		t.Context(), &swapclientrpc.StartReceiveRequest{
			AmountSat: 50_000,
		},
	)
	require.NoError(t, err)
	require.Equal(
		t, hex.EncodeToString(receiveHash[:]), resp.GetPaymentHash(),
	)
	require.Equal(t, "lnbc1receive", resp.GetInvoice())

	fakeClient.awaitReceiveResume(t, receiveHash)
	require.Equal(t, 1, fakeClient.startReceiveCount())
	require.Equal(t, 1, fakeClient.receiveResumeCount(receiveHash))
}

func TestResumeSwapValidatesPaymentHashAndDirection(t *testing.T) {
	t.Parallel()

	hash := testHash(5)
	service := newTestSwapClientService(
		newFakeSwapRuntime(
			swaps.SwapSummary{
				Direction:   swaps.SwapDirectionPay,
				PaymentHash: hash,
				State:       "created",
				Pending:     true,
			},
		),
	)
	defer service.cancel()

	_, err := service.ResumeSwap(
		t.Context(), &swapclientrpc.ResumeSwapRequest{
			PaymentHash: "not-hex",
			Direction: swapclientrpc.
				SwapDirection_SWAP_DIRECTION_PAY,
		},
	)
	require.Error(t, err)

	_, err = service.ResumeSwap(
		t.Context(), &swapclientrpc.ResumeSwapRequest{
			PaymentHash: hex.EncodeToString(hash[:]),
		},
	)
	require.Error(t, err)
}

func TestStartRejectsReservedIdempotencyKey(t *testing.T) {
	t.Parallel()

	service := newTestSwapClientService(newFakeSwapRuntime())
	defer service.cancel()

	_, err := service.StartPay(
		t.Context(), &swapclientrpc.StartPayRequest{
			Invoice:        "lnbc1test",
			IdempotencyKey: "future-key",
		},
	)
	require.Error(t, err)

	_, err = service.StartReceive(
		t.Context(), &swapclientrpc.StartReceiveRequest{
			AmountSat:      50_000,
			IdempotencyKey: "future-key",
		},
	)
	require.Error(t, err)
}

func TestSwapSummaryToProtoCopiesDurableFields(t *testing.T) {
	t.Parallel()

	hash := testHash(6)
	createdAt := time.Unix(100, 0)
	updatedAt := time.Unix(200, 0)
	deadline := time.Unix(300, 0)

	got := swapSummaryToProto(swaps.SwapSummary{
		Direction:        swaps.SwapDirectionReceive,
		PaymentHash:      hash,
		State:            "Completed",
		Pending:          false,
		AmountSat:        1_000,
		FeeSat:           20,
		MaxFeeSat:        30,
		VHTLCOutpoint:    "txid:0",
		VHTLCAmountSat:   990,
		FundingSessionID: "funding",
		ClaimSessionID:   "claim",
		RefundSessionID:  "refund",
		TerminalReason:   "done",
		CreatedAt:        createdAt,
		UpdatedAt:        updatedAt,
		Deadline:         deadline,
		RefundLocktime:   42,
	})

	require.Equal(
		t, swapclientrpc.SwapDirection_SWAP_DIRECTION_RECEIVE,
		got.GetDirection(),
	)
	require.Equal(t, hex.EncodeToString(hash[:]), got.GetPaymentHash())
	require.Equal(
		t, swapclientrpc.SwapState_SWAP_STATE_COMPLETED, got.GetState(),
	)
	require.False(t, got.GetPending())
	require.Equal(t, int64(1_000), got.GetAmountSat())
	require.Equal(t, uint64(20), got.GetFeeSat())
	require.Equal(t, uint64(30), got.GetMaxFeeSat())
	require.Equal(t, "txid:0", got.GetVhtlcOutpoint())
	require.Equal(t, int64(990), got.GetVhtlcAmountSat())
	require.Equal(t, "funding", got.GetFundingSessionId())
	require.Equal(t, "claim", got.GetClaimSessionId())
	require.Equal(t, "refund", got.GetRefundSessionId())
	require.Equal(t, "done", got.GetTerminalReason())
	require.Equal(t, createdAt.Unix(), got.GetCreatedAtUnix())
	require.Equal(t, updatedAt.Unix(), got.GetUpdatedAtUnix())
	require.Equal(t, deadline.Unix(), got.GetDeadlineUnix())
	require.Equal(t, uint32(42), got.GetRefundLocktime())
}

func TestNewSwapClientServiceUsesDaemonBackedReceiveAuth(t *testing.T) {
	t.Parallel()

	rpcServer := darepod.NewRPCServer(nil)
	daemonCfg := &darepod.Config{
		DataDir: t.TempDir(),
		Network: "regtest",
		Swap: &darepod.SwapConfig{
			ServerAddress:  "localhost:10030",
			ServerInsecure: true,
		},
	}

	service, cleanup, err := newSwapClientService(
		t.Context(), rpcServer, daemonCfg,
	)
	require.NoError(t, err)
	require.NotNil(t, service)
	require.NotNil(t, cleanup)
	t.Cleanup(cleanup)

	_, ok := service.client.(*swapClientAdapter)
	require.True(t, ok)
}

func newTestSwapClientService(client swapRuntimeClient) *swapClientService {
	ctx, cancel := context.WithCancel(context.Background())

	return &swapClientService{
		client:      client,
		log:         btclog.Disabled,
		rootCtx:     ctx,
		cancel:      cancel,
		active:      make(map[string]struct{}),
		subscribers: make(map[chan *swapclientrpc.SwapSummary]struct{}),
	}
}

func testHash(seed byte) lntypes.Hash {
	var hash lntypes.Hash
	for i := range hash {
		hash[i] = seed
	}

	return hash
}

type fakeSwapRuntime struct {
	mu sync.Mutex

	summaries []swaps.SwapSummary

	startPaySession     paySwapSession
	startReceiveSession receiveSwapSession

	startPayCalls      int
	startReceiveCalls  int
	getSummaryCalls    int
	listPendingOnly    []bool
	payResumeCalls     map[lntypes.Hash]int
	receiveResumeCalls map[lntypes.Hash]int

	payResumeCh     chan lntypes.Hash
	receiveResumeCh chan lntypes.Hash
}

func newFakeSwapRuntime(summaries ...swaps.SwapSummary) *fakeSwapRuntime {
	return &fakeSwapRuntime{
		summaries:          summaries,
		payResumeCalls:     make(map[lntypes.Hash]int),
		receiveResumeCalls: make(map[lntypes.Hash]int),
		payResumeCh:        make(chan lntypes.Hash, 8),
		receiveResumeCh:    make(chan lntypes.Hash, 8),
	}
}

func (f *fakeSwapRuntime) StartPayViaLightning(context.Context, string,
	uint64) (paySwapSession, error) {

	f.mu.Lock()
	defer f.mu.Unlock()

	f.startPayCalls++
	if f.startPaySession == nil {
		return nil, errors.New("start pay session not configured")
	}

	return f.startPaySession, nil
}

func (f *fakeSwapRuntime) StartReceiveViaLightning(context.Context,
	btcutil.Amount) (receiveSwapSession, error) {

	f.mu.Lock()
	defer f.mu.Unlock()

	f.startReceiveCalls++
	if f.startReceiveSession == nil {
		return nil, errors.New("start receive session not configured")
	}

	return f.startReceiveSession, nil
}

func (f *fakeSwapRuntime) ResumePayViaLightning(_ context.Context,
	hash lntypes.Hash) (paySwapSession, error) {

	f.mu.Lock()
	defer f.mu.Unlock()

	f.payResumeCalls[hash]++
	f.payResumeCh <- hash

	return &fakePaySession{hash: hash}, nil
}

func (f *fakeSwapRuntime) ResumeReceiveViaLightning(_ context.Context,
	hash lntypes.Hash) (receiveSwapSession, error) {

	f.mu.Lock()
	defer f.mu.Unlock()

	f.receiveResumeCalls[hash]++
	f.receiveResumeCh <- hash

	return &fakeReceiveSession{hash: hash}, nil
}

func (f *fakeSwapRuntime) GetSwapSummary(_ context.Context, hash lntypes.Hash) (
	swaps.SwapSummary, error) {

	f.mu.Lock()
	defer f.mu.Unlock()

	f.getSummaryCalls++
	for _, summary := range f.summaries {
		if summary.PaymentHash == hash {
			return summary, nil
		}
	}

	return swaps.SwapSummary{}, swaps.ErrSwapSummaryNotFound
}

func (f *fakeSwapRuntime) ListSwapSummaries(_ context.Context,
	pendingOnly bool) ([]swaps.SwapSummary, error) {

	f.mu.Lock()
	defer f.mu.Unlock()

	f.listPendingOnly = append(f.listPendingOnly, pendingOnly)

	summaries := make([]swaps.SwapSummary, 0, len(f.summaries))
	for _, summary := range f.summaries {
		if pendingOnly && !summary.Pending {
			continue
		}

		summaries = append(summaries, summary)
	}

	return summaries, nil
}

func (f *fakeSwapRuntime) awaitPayResume(t *testing.T, hash lntypes.Hash) {
	t.Helper()

	select {
	case got := <-f.payResumeCh:
		require.Equal(t, hash, got)

	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for pay resume")
	}
}

func (f *fakeSwapRuntime) awaitReceiveResume(t *testing.T, hash lntypes.Hash) {
	t.Helper()

	select {
	case got := <-f.receiveResumeCh:
		require.Equal(t, hash, got)

	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for receive resume")
	}
}

func (f *fakeSwapRuntime) startPayCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.startPayCalls
}

func (f *fakeSwapRuntime) startReceiveCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.startReceiveCalls
}

func (f *fakeSwapRuntime) payResumeCount(hash lntypes.Hash) int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.payResumeCalls[hash]
}

func (f *fakeSwapRuntime) receiveResumeCount(hash lntypes.Hash) int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.receiveResumeCalls[hash]
}

func (f *fakeSwapRuntime) sawPendingOnlyList() bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	for _, pendingOnly := range f.listPendingOnly {
		if pendingOnly {
			return true
		}
	}

	return false
}

type fakePaySession struct {
	hash lntypes.Hash
}

func (f *fakePaySession) PaymentHash() lntypes.Hash {
	return f.hash
}

func (f *fakePaySession) Wait(ctx context.Context) (*swaps.PayResult, error) {
	<-ctx.Done()

	return nil, ctx.Err()
}

type fakeReceiveSession struct {
	hash    lntypes.Hash
	invoice string
}

func (f *fakeReceiveSession) PaymentHash() lntypes.Hash {
	return f.hash
}

func (f *fakeReceiveSession) Invoice() string {
	return f.invoice
}

func (f *fakeReceiveSession) Wait(ctx context.Context) (*swaps.ReceiveResult,
	error) {

	<-ctx.Done()

	return nil, ctx.Err()
}
