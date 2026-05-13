package wallet

import (
	"context"
	"errors"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/chainsource"
	"github.com/lightninglabs/darepo-client/ledger"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// mockSweepChainSourceBehavior extends the basic mockChainSourceBehavior
// pattern with the Asks the boarding-sweep actor handler issues:
// BestHeightRequest, FeeEstimateRequest, and RegisterSpendRequest. The
// boarding-sweep tests reuse this behavior so the underlying actor
// queue serialises Asks the same way the production chainsource actor
// would.
type mockSweepChainSourceBehavior struct {
	bestHeight  int32
	feeRate     btcutil.Amount
	feeRateErr  error
	bestHeightE error
}

// Receive dispatches the supported chainsource messages to canned
// responses. Unknown requests fall through to an error so a missing
// fixture surfaces loudly instead of silently no-oping.
func (m *mockSweepChainSourceBehavior) Receive(_ context.Context,
	msg chainsource.ChainSourceMsg,
) fn.Result[chainsource.ChainSourceResp] {

	switch msg.(type) {
	case *chainsource.BestHeightRequest:
		if m.bestHeightE != nil {
			return fn.Err[chainsource.ChainSourceResp](
				m.bestHeightE,
			)
		}

		return fn.Ok[chainsource.ChainSourceResp](
			&chainsource.BestHeightResponse{
				Height: m.bestHeight,
			},
		)

	case *chainsource.FeeEstimateRequest:
		if m.feeRateErr != nil {
			return fn.Err[chainsource.ChainSourceResp](
				m.feeRateErr,
			)
		}

		return fn.Ok[chainsource.ChainSourceResp](
			&chainsource.FeeEstimateResponse{
				SatPerVByte: m.feeRate,
			},
		)

	case *chainsource.RegisterSpendRequest:
		return fn.Ok[chainsource.ChainSourceResp](
			&chainsource.RegisterSpendResponse{},
		)

	case *chainsource.UnregisterSpendRequest:
		return fn.Ok[chainsource.ChainSourceResp](
			&chainsource.UnregisterSpendResponse{},
		)
	}

	return fn.Err[chainsource.ChainSourceResp](
		errors.New("unknown chainsource msg in test"),
	)
}

// chainSourceRef is a shorthand for the verbose actor.ActorRef typed for
// chainsource so test helpers can return the type without exceeding the
// 80-char line limit.
type chainSourceRef = actor.ActorRef[
	chainsource.ChainSourceMsg, chainsource.ChainSourceResp,
]

// newMockSweepChainSource constructs a chainsource actor ref backed by a
// mockSweepChainSourceBehavior with the supplied canned responses.
func newMockSweepChainSource(t *testing.T, bestHeight int32,
	feeRate btcutil.Amount) chainSourceRef {

	t.Helper()

	a := actor.NewActor(actor.ActorConfig[chainsource.ChainSourceMsg,
		chainsource.ChainSourceResp]{
		ID: "mock-chainsource-sweep",
		Behavior: &mockSweepChainSourceBehavior{
			bestHeight: bestHeight,
			feeRate:    feeRate,
		},
		MailboxSize: 16,
	})
	a.Start()
	t.Cleanup(a.Stop)

	return a.Ref()
}

// MockBoardingSweepStore is a stub BoardingSweepStore for unit tests.
type MockBoardingSweepStore struct {
	mock.Mock
}

func (m *MockBoardingSweepStore) CreatePendingBoardingSweep(ctx context.Context,
	sweep NewBoardingSweep) error {

	args := m.Called(ctx, sweep)

	return args.Error(0)
}

func (m *MockBoardingSweepStore) MarkBoardingSweepPublished(ctx context.Context,
	txid chainhash.Hash) error {

	args := m.Called(ctx, txid)

	return args.Error(0)
}

func (m *MockBoardingSweepStore) MarkBoardingSweepFailed(ctx context.Context,
	txid chainhash.Hash, failure error) error {

	args := m.Called(ctx, txid, failure)

	return args.Error(0)
}

func (m *MockBoardingSweepStore) MarkBoardingSweepInputSpent(
	ctx context.Context, outpoint wire.OutPoint,
	spendingTxid chainhash.Hash, spendingHeight int32) (bool, error) {

	args := m.Called(ctx, outpoint, spendingTxid, spendingHeight)

	return args.Bool(0), args.Error(1)
}

func (m *MockBoardingSweepStore) ListBoardingSweeps(ctx context.Context,
	status string, limit, offset int32) ([]BoardingSweepRecord, error) {

	args := m.Called(ctx, status, limit, offset)
	if v := args.Get(0); v != nil {
		records, _ := v.([]BoardingSweepRecord)

		return records, args.Error(1)
	}

	return nil, args.Error(1)
}

func (m *MockBoardingSweepStore) ListPendingBoardingSweeps(
	ctx context.Context) ([]BoardingSweepRecord, error) {

	args := m.Called(ctx)
	if v := args.Get(0); v != nil {
		records, _ := v.([]BoardingSweepRecord)

		return records, args.Error(1)
	}

	return nil, args.Error(1)
}

func (m *MockBoardingSweepStore) GetBoardingSweep(ctx context.Context,
	txid chainhash.Hash) (*BoardingSweepRecord, error) {

	args := m.Called(ctx, txid)
	if v := args.Get(0); v != nil {
		record, _ := v.(*BoardingSweepRecord)

		return record, args.Error(1)
	}

	return nil, args.Error(1)
}

func (m *MockBoardingSweepStore) FetchBoardingIntentsBySweepableStatuses(
	ctx context.Context, statuses []BoardingStatus) ([]BoardingIntent,
	error) {

	args := m.Called(ctx, statuses)
	if v := args.Get(0); v != nil {
		intents, _ := v.([]BoardingIntent)

		return intents, args.Error(1)
	}

	return nil, args.Error(1)
}

func (m *MockBoardingSweepStore) GetIntent(ctx context.Context,
	outpoint wire.OutPoint) (*BoardingIntent, error) {

	args := m.Called(ctx, outpoint)
	if v := args.Get(0); v != nil {
		intent, _ := v.(*BoardingIntent)

		return intent, args.Error(1)
	}

	return nil, args.Error(1)
}

// newSweepTestArk wires a wallet.Ark with a mock chainsource Ask surface,
// the supplied boarding-sweep store, a deterministic sweep signer, and an
// otherwise inert wallet actor configuration. The intent is to exercise
// the sweep-handler logic in isolation without booting a full daemon.
//
// signer may be nil when the test only exercises paths that do not touch
// signing (preview-with-zero-candidates, ListBoardingSweeps, request
// validation guards).
func newSweepTestArk(t *testing.T, store BoardingSweepStore, signer SweepSigner,
	bestHeight int32, feeRate btcutil.Amount) *Ark {

	t.Helper()

	chainSource := newMockSweepChainSource(t, bestHeight, feeRate)
	walletStore := &MockBoardingStore{}
	backend := &MockBoardingBackend{}

	a := NewArk(
		backend, walletStore, nil, chainSource, nil,
		fn.None[ledger.Sink](), btclog.Disabled, WithBoardingSweep(
			store, signer, &chaincfg.RegressionNetParams,
		),
	)

	return a
}

// TestSweepBoardingUTXOsRejectsNegativeFeeRate verifies the actor rejects
// a request that supplies an invalid (negative) fee rate up-front, before
// loading candidates or asking the chainsource for fee estimates.
func TestSweepBoardingUTXOsRejectsNegativeFeeRate(t *testing.T) {
	t.Parallel()

	store := &MockBoardingSweepStore{}
	a := newSweepTestArk(
		t, store, &testBoardingSweepWallet{}, 200, 2,
	)

	req := &SweepBoardingUTXOsRequest{
		FeeRateSatPerVByte: -1,
	}

	result := a.handleSweepBoardingUTXOs(t.Context(), req)
	require.True(t, result.IsErr())
	require.ErrorContains(t, result.Err(), "must be non-negative")

	// No store calls should have been issued before the validation
	// guard fired.
	store.AssertNotCalled(
		t, "FetchBoardingIntentsBySweepableStatuses",
	)
}

// TestSweepBoardingUTXOsSubsystemDisabled verifies that a wallet actor
// constructed without WithBoardingSweep returns a clear error rather than
// silently no-oping when a sweep request arrives.
func TestSweepBoardingUTXOsSubsystemDisabled(t *testing.T) {
	t.Parallel()

	chainSource := newMockSweepChainSource(t, 200, 2)
	a := NewArk(
		&MockBoardingBackend{}, &MockBoardingStore{}, nil,
		chainSource, nil, fn.None[ledger.Sink](),
		btclog.Disabled,
		// No WithBoardingSweep — sweep subsystem disabled.
	)

	result := a.handleSweepBoardingUTXOs(
		t.Context(), &SweepBoardingUTXOsRequest{},
	)
	require.True(t, result.IsErr())
	require.ErrorContains(t, result.Err(), "not initialised")
}

// TestSweepBoardingUTXOsEmptyPreview verifies the actor returns a
// preview response with zero totals and no built tx when no boarding
// intents satisfy the sweepable-status filter.
func TestSweepBoardingUTXOsEmptyPreview(t *testing.T) {
	t.Parallel()

	store := &MockBoardingSweepStore{}
	store.On(
		"FetchBoardingIntentsBySweepableStatuses",
		mock.Anything, mock.Anything,
	).Return([]BoardingIntent{}, nil)

	a := newSweepTestArk(
		t, store, &testBoardingSweepWallet{}, 200, 2,
	)

	result := a.handleSweepBoardingUTXOs(
		t.Context(), &SweepBoardingUTXOsRequest{
			Broadcast: false,
		},
	)
	require.True(t, result.IsOk())

	respVal, _ := result.Unpack()
	resp, ok := respVal.(*SweepBoardingUTXOsResponse)
	require.True(t, ok)
	require.Equal(t, "preview", resp.Status)
	require.False(t, resp.HasTxid)
	require.Equal(t, int64(0), resp.TotalAmountSat)
	require.Empty(t, resp.SweepableOutputs)

	store.AssertExpectations(t)
}

// TestSweepBoardingUTXOsImmatureCandidatesNotIncluded verifies that
// boarding intents whose CSV maturity height is still in the future are
// excluded from the preview response — the actor returns an empty
// preview rather than building a sweep that the chain would reject.
func TestSweepBoardingUTXOsImmatureCandidatesNotIncluded(t *testing.T) {
	t.Parallel()

	intent := testBoardingSweepIntent(t, 50_000, 100, 144)

	store := &MockBoardingSweepStore{}
	store.On(
		"FetchBoardingIntentsBySweepableStatuses",
		mock.Anything, mock.Anything,
	).Return([]BoardingIntent{intent}, nil)

	// bestHeight (200) < maturity (100 + 144 = 244): not yet
	// spendable via the timeout path.
	a := newSweepTestArk(
		t, store, &testBoardingSweepWallet{}, 200, 2,
	)

	result := a.handleSweepBoardingUTXOs(
		t.Context(), &SweepBoardingUTXOsRequest{
			Broadcast: false,
		},
	)
	require.True(t, result.IsOk())

	respVal, _ := result.Unpack()
	resp := respVal.(*SweepBoardingUTXOsResponse) //nolint:forcetypeassert
	require.Equal(t, "preview", resp.Status)
	require.False(t, resp.HasTxid)
	require.Empty(t, resp.SweepableOutputs)
}

// TestSweepBoardingUTXOsPreviewBuildsTx verifies that a request with
// mature candidates and Broadcast=false builds and signs the sweep
// preview without persisting anything to the store.
func TestSweepBoardingUTXOsPreviewBuildsTx(t *testing.T) {
	t.Parallel()

	intent := testBoardingSweepIntent(t, 50_000, 100, 10)

	store := &MockBoardingSweepStore{}
	store.On(
		"FetchBoardingIntentsBySweepableStatuses",
		mock.Anything, mock.Anything,
	).Return([]BoardingIntent{intent}, nil)

	a := newSweepTestArk(
		t, store, &testBoardingSweepWallet{}, 200, 2,
	)

	result := a.handleSweepBoardingUTXOs(
		t.Context(), &SweepBoardingUTXOsRequest{
			Broadcast: false,
		},
	)
	require.True(t, result.IsOk())

	respVal, _ := result.Unpack()
	resp := respVal.(*SweepBoardingUTXOsResponse) //nolint:forcetypeassert
	require.Equal(t, "preview", resp.Status)
	require.True(t, resp.HasTxid)
	require.NotZero(t, resp.EstimatedFeeSat)
	require.Equal(t, int64(50_000), resp.TotalAmountSat)
	require.Len(t, resp.SweepableOutputs, 1)

	// Preview must NOT touch the persistence layer.
	store.AssertNotCalled(
		t, "CreatePendingBoardingSweep", mock.Anything, mock.Anything,
	)
	store.AssertNotCalled(
		t, "MarkBoardingSweepPublished", mock.Anything, mock.Anything,
	)
}

// TestListBoardingSweeps verifies the actor delegates a list request to
// the boarding-sweep store and surfaces the records in the response.
func TestListBoardingSweeps(t *testing.T) {
	t.Parallel()

	want := []BoardingSweepRecord{
		{
			Status: BoardingSweepStatusConfirmed,
		},
		{
			Status: BoardingSweepStatusPublished,
		},
	}

	store := &MockBoardingSweepStore{}
	store.On(
		"ListBoardingSweeps", mock.Anything, "", int32(100),
		int32(0),
	).Return(want, nil)

	a := newSweepTestArk(t, store, nil, 0, 0)

	result := a.handleListBoardingSweeps(
		t.Context(), &ListBoardingSweepsRequest{},
	)
	require.True(t, result.IsOk())

	respVal, _ := result.Unpack()
	resp := respVal.(*ListBoardingSweepsResponse) //nolint:forcetypeassert
	require.Len(t, resp.Records, len(want))
	require.Equal(t, want[0].Status, resp.Records[0].Status)
	require.Equal(t, want[1].Status, resp.Records[1].Status)

	store.AssertExpectations(t)
}

// TestListBoardingSweepsRejectsInvalidStatusFilter verifies the actor
// rejects a list request that names an unrecognised status.
func TestListBoardingSweepsRejectsInvalidStatusFilter(t *testing.T) {
	t.Parallel()

	store := &MockBoardingSweepStore{}
	a := newSweepTestArk(t, store, nil, 0, 0)

	result := a.handleListBoardingSweeps(t.Context(),
		&ListBoardingSweepsRequest{
			StatusFilter: "not-a-real-status",
		})
	require.True(t, result.IsErr())
	require.ErrorContains(t, result.Err(), "invalid status filter")

	store.AssertNotCalled(
		t, "ListBoardingSweeps", mock.Anything, mock.Anything,
		mock.Anything, mock.Anything,
	)
}

// TestSweepSpendNotificationMarksInputSpent verifies that a chainsource
// spend event for a tracked input is forwarded to the store and the
// in-memory tracking map is cleaned up when the sweep resolves.
func TestSweepSpendNotificationMarksInputSpent(t *testing.T) {
	t.Parallel()

	op := wire.OutPoint{Hash: chainhash.Hash{0xab}, Index: 0}
	spendingTxid := chainhash.Hash{0xcd}

	store := &MockBoardingSweepStore{}
	store.On(
		"MarkBoardingSweepInputSpent", mock.Anything, op,
		spendingTxid, int32(220),
	).Return(true, nil)

	a := newSweepTestArk(t, store, nil, 0, 0)

	// Pre-populate pending sweep state so the handler clears it on
	// resolution.
	pendingTxid := chainhash.Hash{0xee}
	a.pendingSweeps[pendingTxid] = &pendingSweepState{
		txid: pendingTxid,
		inputs: map[wire.OutPoint]string{
			op: boardingSweepCallerID(op),
		},
	}

	result := a.handleSweepSpendNotification(
		t.Context(), BoardingSweepSpendNotification{
			Outpoint:       op,
			SpendingTxid:   spendingTxid,
			SpendingHeight: 220,
		},
	)
	require.True(t, result.IsOk())
	require.Empty(
		t, a.pendingSweeps,
		"resolved sweep must be evicted from in-memory tracking",
	)

	store.AssertExpectations(t)
}

// TestSweepTxNotificationFailedMarksFailed verifies that a terminal
// txconfirm failure is mirrored into the persistent store and the
// in-memory pending state is dropped.
func TestSweepTxNotificationFailedMarksFailed(t *testing.T) {
	t.Parallel()

	failedTxid := chainhash.Hash{0x99}
	store := &MockBoardingSweepStore{}
	store.On(
		"MarkBoardingSweepFailed", mock.Anything, failedTxid,
		mock.Anything,
	).Return(nil)

	a := newSweepTestArk(t, store, nil, 0, 0)
	a.pendingSweeps[failedTxid] = &pendingSweepState{
		txid:   failedTxid,
		inputs: map[wire.OutPoint]string{},
	}

	result := a.handleSweepTxNotification(
		t.Context(), BoardingSweepTxNotification{
			Confirmed: false,
			Txid:      failedTxid,
			Reason:    "test failure",
		},
	)
	require.True(t, result.IsOk())
	require.Empty(t, a.pendingSweeps)

	store.AssertExpectations(t)
}
