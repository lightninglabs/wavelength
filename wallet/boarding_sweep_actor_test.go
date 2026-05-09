package wallet

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/chainsource"
	"github.com/lightninglabs/darepo-client/ledger"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	"github.com/lightninglabs/darepo-client/lib/tx/arktx"
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

// capturingLedgerBehavior collects ledger messages sent through the
// wallet actor's ledgerSink, so unit tests can assert on the boarding
// sweep emission shape without booting a real ledger actor. The
// internal channel is buffered to absorb the Tell volume one
// confirmation can produce (one fee leg, one UTXOSpentMsg per input
// up to the sweep cap, plus one optional UTXOCreatedMsg).
type capturingLedgerBehavior struct {
	ch chan ledger.LedgerMsg
}

// Receive records the incoming message and returns a nil response
// (LedgerResp is fire-and-forget).
func (c *capturingLedgerBehavior) Receive(_ context.Context,
	msg ledger.LedgerMsg) fn.Result[ledger.LedgerResp] {

	c.ch <- msg

	return fn.Ok[ledger.LedgerResp](nil)
}

// newCapturingLedgerSink starts an in-memory actor backed by
// capturingLedgerBehavior and returns the wallet-side sink plus a
// drain helper. drain blocks until either the requested message count
// is observed or a short test deadline elapses, so callers can write
// the assertion the same way regardless of mailbox scheduling.
func newCapturingLedgerSink(t *testing.T) (ledger.Sink,
	func(want int) []ledger.LedgerMsg) {

	t.Helper()

	const bufferSize = 256

	beh := &capturingLedgerBehavior{
		ch: make(chan ledger.LedgerMsg, bufferSize),
	}
	a := actor.NewActor(actor.ActorConfig[ledger.LedgerMsg,
		ledger.LedgerResp]{
		ID:          "test-ledger-sink",
		Behavior:    beh,
		MailboxSize: bufferSize,
	})
	a.Start()
	t.Cleanup(a.Stop)

	sink := ledger.Sink(a.Ref())

	drain := func(want int) []ledger.LedgerMsg {
		out := make([]ledger.LedgerMsg, 0, want)
		timeout := time.After(2 * time.Second)
		for len(out) < want {
			select {
			case m := <-beh.ch:
				out = append(out, m)

			case <-timeout:
				return out
			}
		}

		// After hitting the expected count, briefly drain any
		// trailing messages so tests asserting "no extras" can
		// catch unintended emissions without false negatives.
		settle := time.After(20 * time.Millisecond)
		for {
			select {
			case m := <-beh.ch:
				out = append(out, m)

			case <-settle:
				return out
			}
		}
	}

	return sink, drain
}

// TestSweepTxNotificationConfirmedEmitsLedger verifies that on a
// confirmed sweep, the wallet actor emits a FeePaidMsg with
// FeeTypeOnchainSweep, one UTXOSpentMsg per swept input, and (because
// the destination is wallet-derived) a UTXOCreatedMsg for the sweep
// destination output.
func TestSweepTxNotificationConfirmedEmitsLedger(t *testing.T) {
	t.Parallel()

	swept := chainhash.Hash{0x42}
	in1 := wire.OutPoint{Hash: chainhash.Hash{0xab}, Index: 0}
	in2 := wire.OutPoint{Hash: chainhash.Hash{0xcd}, Index: 1}

	store := &MockBoardingSweepStore{}

	// emitSweepConfirmedLedger reads the persisted sweep record as the
	// sole source of truth for inputs / destination / amounts, so the
	// mock must return a populated record keyed by the sweep txid. The
	// in-memory pendingSweeps map is intentionally NOT consulted at
	// confirmation time, since handleSweepSpendNotification routinely
	// clears it before the txconfirm Confirmed event arrives.
	const (
		input1Sat       = int64(40_000)
		input2Sat       = int64(60_000)
		feeSat          = int64(444)
		anchorSat       = int64(330)
		walletOutputSat = input1Sat + input2Sat - feeSat - anchorSat
	)
	sweepTx := wire.NewMsgTx(arktx.TxVersion)
	sweepTx.AddTxOut(&wire.TxOut{
		Value:    walletOutputSat,
		PkScript: []byte{txscript.OP_TRUE},
	})
	sweepTx.AddTxOut(
		arkscript.AnchorOutput(
			arkscript.WithAnchorValue(anchorSat),
		),
	)
	walletDerivedRecord := &BoardingSweepRecord{
		Txid:               swept,
		Tx:                 sweepTx,
		DestinationAddress: "", // empty == wallet-derived
		TotalAmount:        btcutil.Amount(input1Sat + input2Sat),
		FeeAmount:          btcutil.Amount(feeSat),
		Status:             "confirmed",
		Inputs: []BoardingSweepInputRecord{
			{
				Txid:     swept,
				Outpoint: in1,
				Amount:   btcutil.Amount(input1Sat),
				Status:   BoardingSweepInputStatusSpent,
			},
			{
				Txid:     swept,
				Outpoint: in2,
				Amount:   btcutil.Amount(input2Sat),
				Status:   BoardingSweepInputStatusSpent,
			},
		},
	}
	store.On(
		"GetBoardingSweep", mock.Anything, swept,
	).Return(walletDerivedRecord, nil)

	chainSource := newMockSweepChainSource(t, 0, 0)
	sink, drain := newCapturingLedgerSink(t)
	a := NewArk(
		&MockBoardingBackend{}, &MockBoardingStore{}, nil, chainSource,
		nil, fn.Some(sink), btclog.Disabled,
		WithBoardingSweep(
			store, &testBoardingSweepWallet{},
			&chaincfg.RegressionNetParams,
		),
	)

	result := a.handleSweepTxNotification(
		t.Context(), BoardingSweepTxNotification{
			Confirmed:   true,
			Txid:        swept,
			BlockHeight: 800_650,
			NumConfs:    1,
		},
	)
	require.True(t, result.IsOk())

	// Expect 4 messages: 1 FeePaidMsg + 2 UTXOSpentMsg + 1 UTXOCreatedMsg.
	msgs := drain(4)
	require.NotEmpty(
		t, msgs, "confirmed sweep must emit at least the fee leg",
	)

	var (
		feePaid     *ledger.FeePaidMsg
		utxoSpent   []*ledger.UTXOSpentMsg
		utxoCreated *ledger.UTXOCreatedMsg
	)
	for _, m := range msgs {
		switch typed := m.(type) {
		case *ledger.FeePaidMsg:
			feePaid = typed

		case *ledger.UTXOSpentMsg:
			utxoSpent = append(utxoSpent, typed)

		case *ledger.UTXOCreatedMsg:
			utxoCreated = typed
		}
	}

	require.NotNil(t, feePaid)
	require.Equal(t, ledger.FeeTypeOnchainSweep, feePaid.FeeType)
	require.Equal(t, feeSat, feePaid.AmountSat)
	require.Equal(t, swept[:], feePaid.IdempotencyKey)

	require.Len(
		t, utxoSpent, 2, "one UTXOSpentMsg per swept boarding input",
	)

	// Per-input AmountSat must reflect the persisted boarding-UTXO
	// value rather than defaulting to zero — otherwise the audit log
	// silently records a 0-sat outflow.
	spentByOutpoint := make(
		map[wire.OutPoint]*ledger.UTXOSpentMsg, len(utxoSpent),
	)
	for _, m := range utxoSpent {
		require.Equal(
			t, ledger.ClassificationBoardingSweepInput,
			m.Classification,
		)
		op := wire.OutPoint{
			Hash:  m.OutpointHash,
			Index: m.OutpointIndex,
		}
		spentByOutpoint[op] = m
	}
	require.NotNil(t, spentByOutpoint[in1])
	require.Equal(t, input1Sat, spentByOutpoint[in1].AmountSat)
	require.NotNil(t, spentByOutpoint[in2])
	require.Equal(t, input2Sat, spentByOutpoint[in2].AmountSat)

	require.NotNil(
		t, utxoCreated,
		"wallet-derived destination must emit one UTXOCreatedMsg",
	)
	require.Equal(
		t, ledger.ClassificationBoardingSweepReturn,
		utxoCreated.Classification,
	)
	require.Equal(t, [32]byte(swept), utxoCreated.OutpointHash)
	require.Equal(t, walletOutputSat, utxoCreated.AmountSat)
}

// TestSweepTxNotificationConfirmedExternalDestSkipsCreated verifies that
// when a sweep was paid to an external (non-wallet) address, the actor
// emits the fee leg and per-input audit rows but skips the destination
// UTXOCreatedMsg — those funds left the wallet entirely and the
// per-input UTXOSpentMsg covers the outflow.
func TestSweepTxNotificationConfirmedExternalDestSkipsCreated(t *testing.T) {
	t.Parallel()

	swept := chainhash.Hash{0x55}
	in1 := wire.OutPoint{Hash: chainhash.Hash{0x11}, Index: 0}

	// A non-empty DestinationAddress on the persisted record marks the
	// sweep as paying to a caller-supplied external address (the
	// persisted equivalent of the in-memory destWalletDerived=false
	// signal). The destination UTXOCreatedMsg must be skipped because
	// the funds left the wallet entirely.
	const (
		inputSat        = int64(40_000)
		feeSat          = int64(222)
		anchorSat       = int64(330)
		externalDestSat = inputSat - feeSat - anchorSat
	)
	externalDestTx := wire.NewMsgTx(arktx.TxVersion)
	externalDestTx.AddTxOut(&wire.TxOut{
		Value:    externalDestSat,
		PkScript: []byte{txscript.OP_TRUE},
	})
	externalDestTx.AddTxOut(
		arkscript.AnchorOutput(
			arkscript.WithAnchorValue(anchorSat),
		),
	)
	externalDestRecord := &BoardingSweepRecord{
		Txid:               swept,
		Tx:                 externalDestTx,
		DestinationAddress: "bcrt1pexternaladdress",
		TotalAmount:        btcutil.Amount(inputSat),
		FeeAmount:          btcutil.Amount(feeSat),
		Status:             "confirmed",
		Inputs: []BoardingSweepInputRecord{
			{
				Txid:     swept,
				Outpoint: in1,
				Amount:   btcutil.Amount(inputSat),
				Status:   BoardingSweepInputStatusSpent,
			},
		},
	}

	store := &MockBoardingSweepStore{}
	store.On(
		"GetBoardingSweep", mock.Anything, swept,
	).Return(externalDestRecord, nil)

	chainSource := newMockSweepChainSource(t, 0, 0)
	sink, drain := newCapturingLedgerSink(t)
	a := NewArk(
		&MockBoardingBackend{}, &MockBoardingStore{}, nil, chainSource,
		nil, fn.Some(sink), btclog.Disabled,
		WithBoardingSweep(
			store, &testBoardingSweepWallet{},
			&chaincfg.RegressionNetParams,
		),
	)

	result := a.handleSweepTxNotification(
		t.Context(), BoardingSweepTxNotification{
			Confirmed:   true,
			Txid:        swept,
			BlockHeight: 800_700,
		},
	)
	require.True(t, result.IsOk())

	// Expect 2 messages: 1 FeePaidMsg + 1 UTXOSpentMsg. drain settles
	// after these, so a stray UTXOCreatedMsg would still be captured.
	msgs := drain(2)
	for _, m := range msgs {
		_, isCreated := m.(*ledger.UTXOCreatedMsg)
		require.False(
			t, isCreated, "external-destination sweep must NOT "+
				"emit UTXOCreatedMsg",
		)
	}
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
