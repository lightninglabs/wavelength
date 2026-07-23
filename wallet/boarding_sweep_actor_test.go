package wallet

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/chainsource"
	"github.com/lightninglabs/wavelength/ledger"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/lib/tx/arktx"
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

// TestSweepSpendNotificationKeepsPositiveProvisional verifies that a
// chainsource spend event remains reversible until the final Done event.
func TestSweepSpendNotificationKeepsPositiveProvisional(t *testing.T) {
	t.Parallel()

	op := wire.OutPoint{Hash: chainhash.Hash{0xab}, Index: 0}
	spendingTxid := chainhash.Hash{0xcd}

	store := &MockBoardingSweepStore{}

	a := newSweepTestArk(t, store, nil, 0, 0)

	// Pre-populate pending sweep state. The positive observation must not
	// clear it or touch durable state before the finality boundary.
	pendingTxid := chainhash.Hash{0xee}
	pending := &pendingSweepState{
		txid: pendingTxid,
		inputs: map[wire.OutPoint]string{
			op: boardingSweepCallerID(op),
		},
		provisionalSpends: map[wire.OutPoint]provisionalSweepSpend{
			op: {
				txid: chainhash.Hash{
					0xb3,
				},
				height: 120,
			},
		},
	}
	a.pendingSweeps[pendingTxid] = pending
	a.pendingSweepInputs[op] = pendingTxid

	result := a.handleSweepSpendNotification(
		t.Context(), BoardingSweepSpendNotification{
			Status:         BoardingSweepSpendStatusSpent,
			Outpoint:       op,
			SpendingTxid:   spendingTxid,
			SpendingHeight: 220,
		},
	)
	require.True(t, result.IsOk())
	require.Same(t, pending, a.pendingSweeps[pendingTxid])
	require.Equal(t, pendingTxid, a.pendingSweepInputs[op])
	require.Equal(
		t, provisionalSweepSpend{
			txid: spendingTxid, height: 220,
		}, pending.provisionalSpends[op],
	)
	store.AssertNotCalled(t, "MarkBoardingSweepInputSpent")
}

// TestSweepSpendNotificationDefersOwnSweepUntilFinality verifies that the
// legacy per-input watch cannot make the sweep terminal on its first
// confirmation. txconfirm's Finalized lifecycle event owns that transition.
func TestSweepSpendNotificationDefersOwnSweepUntilFinality(t *testing.T) {
	t.Parallel()

	op := wire.OutPoint{Hash: chainhash.Hash{0xac}, Index: 1}
	sweepTxid := chainhash.Hash{0xef}
	store := &MockBoardingSweepStore{}
	a := newSweepTestArk(t, store, nil, 0, 0)

	pending := &pendingSweepState{
		txid: sweepTxid,
		inputs: map[wire.OutPoint]string{
			op: boardingSweepCallerID(op),
		},
	}
	a.pendingSweeps[sweepTxid] = pending
	a.pendingSweepInputs[op] = sweepTxid

	result := a.handleSweepSpendNotification(
		t.Context(), BoardingSweepSpendNotification{
			Status:         BoardingSweepSpendStatusSpent,
			Outpoint:       op,
			SpendingTxid:   sweepTxid,
			SpendingHeight: 220,
		},
	)
	require.True(t, result.IsOk())
	require.Same(t, pending, a.pendingSweeps[sweepTxid])
	require.Equal(t, sweepTxid, a.pendingSweepInputs[op])
	require.Equal(
		t, provisionalSweepSpend{
			txid: sweepTxid, height: 220,
		}, pending.provisionalSpends[op],
	)
	store.AssertNotCalled(t, "MarkBoardingSweepInputSpent")
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

// TestSweepTxNotificationFinalizedEmitsLedger verifies that once a sweep
// reaches policy finality, the wallet actor emits a FeePaidMsg with
// FeeTypeOnchainSweep, one UTXOSpentMsg per swept input, and (because
// the destination is wallet-derived) a UTXOCreatedMsg for the sweep
// destination output.
func TestSweepTxNotificationFinalizedEmitsLedger(t *testing.T) {
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
			Status:      BoardingSweepTxStatusFinalized,
			Txid:        swept,
			BlockHeight: 800_650,
			NumConfs:    1,
		},
	)
	require.True(t, result.IsOk())

	// The finalized sweep emits exactly one consolidated message so
	// the ledger books every clearing leg atomically.
	msgs := drain(1)
	require.Len(t, msgs, 1)

	confirmed, ok := msgs[0].(*ledger.BoardingSweepConfirmedMsg)
	require.True(
		t, ok, "confirmed sweep must emit a BoardingSweepConfirmedMsg",
	)

	require.Equal(t, [32]byte(swept), confirmed.Txid)
	require.Equal(t, uint32(800_650), confirmed.BlockHeight)

	// Chain cost is miner fee + P2A anchor.
	require.Equal(t, feeSat+anchorSat, confirmed.ChainCostSat)

	// Wallet-derived destination: not external, value at vout 0.
	require.False(t, confirmed.DestinationExternal)
	require.Equal(t, walletOutputSat, confirmed.DestinationSat)

	// Per-input amounts must reflect the persisted boarding-UTXO values.
	require.Len(
		t, confirmed.Inputs, 2,
		"one sweep input per swept boarding outpoint",
	)
	amtByOutpoint := make(
		map[wire.OutPoint]int64, len(confirmed.Inputs),
	)
	for _, in := range confirmed.Inputs {
		amtByOutpoint[in.Outpoint] = in.AmountSat
	}
	require.Equal(t, input1Sat, amtByOutpoint[in1])
	require.Equal(t, input2Sat, amtByOutpoint[in2])

	// The clearing identity must hold: inputs - chain cost - dest == 0.
	var inputsTotal int64
	for _, in := range confirmed.Inputs {
		inputsTotal += in.AmountSat
	}
	require.Equal(
		t, int64(0),
		inputsTotal-confirmed.ChainCostSat-confirmed.DestinationSat,
		"sweep clearing identity must balance",
	)
}

// TestSweepTxNotificationFinalizedExternalDestSkipsCreated verifies that
// when a finalized sweep was paid to an external (non-wallet) address, the
// consolidated message marks the destination external so the ledger settles
// it to transfers_out rather than booking a wallet-return deposit.
func TestSweepTxNotificationFinalizedExternalDestSkipsCreated(t *testing.T) {
	t.Parallel()

	swept := chainhash.Hash{0x55}
	in1 := wire.OutPoint{Hash: chainhash.Hash{0x11}, Index: 0}

	// A non-empty DestinationAddress on the persisted record marks the
	// sweep as paying to a caller-supplied external address (the
	// persisted equivalent of the in-memory destWalletDerived=false
	// signal). The consolidated message must flag DestinationExternal so
	// the value settles to transfers_out, not a wallet-return deposit.
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
			Status:      BoardingSweepTxStatusFinalized,
			Txid:        swept,
			BlockHeight: 800_700,
		},
	)
	require.True(t, result.IsOk())

	// One consolidated message, flagged external so the ledger books the
	// destination to transfers_out instead of a wallet-return deposit.
	msgs := drain(1)
	require.Len(t, msgs, 1)

	confirmed, ok := msgs[0].(*ledger.BoardingSweepConfirmedMsg)
	require.True(t, ok)
	require.True(
		t, confirmed.DestinationExternal,
		"external-destination sweep must flag DestinationExternal",
	)
	require.Equal(t, externalDestSat, confirmed.DestinationSat)
	require.Len(t, confirmed.Inputs, 1)
	require.Equal(t, in1, confirmed.Inputs[0].Outpoint)
	require.Equal(t, inputSat, confirmed.Inputs[0].AmountSat)
}

// spentSweepInput builds a spent boarding-sweep input record for tests.
func spentSweepInput(txid chainhash.Hash, op wire.OutPoint,
	amt int64) BoardingSweepInputRecord {

	return BoardingSweepInputRecord{
		Txid:     txid,
		Outpoint: op,
		Amount:   btcutil.Amount(amt),
		Status:   BoardingSweepInputStatusSpent,
	}
}

// TestSweepLedgerClearingNetsToZero locks in the core accounting invariant
// for boarding-sweep confirmation: the single consolidated message must
// carry amounts whose clearing identity (Σ inputs − chain cost −
// destination) nets to zero. It exercises both the wallet-derived return
// path and the external-destination path. The chain cost is
// (total − destination) = miner fee + anchor, so the inputs debit and the
// fee + destination credits cancel exactly when the ledger books them.
func TestSweepLedgerClearingNetsToZero(t *testing.T) {
	t.Parallel()

	const (
		input1Sat = int64(40_000)
		input2Sat = int64(60_000)
		feeSat    = int64(444)
		anchorSat = int64(330)
	)

	buildTx := func(destSat int64) *wire.MsgTx {
		tx := wire.NewMsgTx(arktx.TxVersion)
		tx.AddTxOut(&wire.TxOut{
			Value:    destSat,
			PkScript: []byte{txscript.OP_TRUE},
		})
		tx.AddTxOut(
			arkscript.AnchorOutput(
				arkscript.WithAnchorValue(anchorSat),
			),
		)

		return tx
	}

	cases := []struct {
		name         string
		destAddr     string
		wantExternal bool
	}{
		{
			name:         "wallet-derived return",
			destAddr:     "",
			wantExternal: false,
		},
		{
			name:         "external destination",
			destAddr:     "bcrt1pexternaladdress",
			wantExternal: true,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			swept := chainhash.Hash{0x7a}
			in1 := wire.OutPoint{
				Hash: chainhash.Hash{
					0xa1,
				}, Index: 0,
			}
			in2 := wire.OutPoint{
				Hash: chainhash.Hash{
					0xb2,
				}, Index: 1,
			}

			total := input1Sat + input2Sat
			destSat := total - feeSat - anchorSat

			record := &BoardingSweepRecord{
				Txid:               swept,
				Tx:                 buildTx(destSat),
				DestinationAddress: tc.destAddr,
				TotalAmount:        btcutil.Amount(total),
				FeeAmount:          btcutil.Amount(feeSat),
				Status:             "confirmed",
				Inputs: []BoardingSweepInputRecord{
					spentSweepInput(swept, in1, input1Sat),
					spentSweepInput(swept, in2, input2Sat),
				},
			}

			store := &MockBoardingSweepStore{}
			store.On(
				"GetBoardingSweep", mock.Anything, swept,
			).Return(record, nil)

			chainSource := newMockSweepChainSource(t, 0, 0)
			sink, drain := newCapturingLedgerSink(t)
			a := NewArk(
				&MockBoardingBackend{}, &MockBoardingStore{},
				nil, chainSource, nil, fn.Some(sink),
				btclog.Disabled,
				WithBoardingSweep(
					store, &testBoardingSweepWallet{},
					&chaincfg.RegressionNetParams,
				),
			)

			notif := BoardingSweepTxNotification{
				Status:      BoardingSweepTxStatusFinalized,
				Txid:        swept,
				BlockHeight: 800_800,
			}
			result := a.handleSweepTxNotification(
				t.Context(), notif,
			)
			require.True(t, result.IsOk())

			msgs := drain(1)
			require.Len(t, msgs, 1)

			got := msgs[0]
			confirmed, ok := got.(*ledger.BoardingSweepConfirmedMsg)
			require.True(t, ok)
			require.Equal(
				t, tc.wantExternal,
				confirmed.DestinationExternal,
			)

			var inputsTotal int64
			for _, in := range confirmed.Inputs {
				inputsTotal += in.AmountSat
			}
			require.Equal(
				t, int64(0),
				inputsTotal-confirmed.ChainCostSat-
					confirmed.DestinationSat,
				"sweep clearing identity must balance",
			)
		})
	}
}

// TestSweepTxNotificationMissingTxSkipsLegs verifies that a sweep record
// without its persisted transaction emits NO clearing legs at all, rather
// than booking a fee + input set whose destination leg cannot be computed.
// Emitting a partial set would strand the destination value in
// wallet_clearing forever; skipping leaves the account untouched at zero.
func TestSweepTxNotificationMissingTxSkipsLegs(t *testing.T) {
	t.Parallel()

	swept := chainhash.Hash{0x6c}
	in1 := wire.OutPoint{Hash: chainhash.Hash{0xd4}, Index: 0}

	record := &BoardingSweepRecord{
		Txid:               swept,
		Tx:                 nil,
		DestinationAddress: "",
		TotalAmount:        btcutil.Amount(40_000),
		FeeAmount:          btcutil.Amount(500),
		Status:             "confirmed",
		Inputs: []BoardingSweepInputRecord{
			{
				Txid:     swept,
				Outpoint: in1,
				Amount:   btcutil.Amount(40_000),
				Status:   BoardingSweepInputStatusSpent,
			},
		},
	}

	store := &MockBoardingSweepStore{}
	store.On(
		"GetBoardingSweep", mock.Anything, swept,
	).Return(record, nil)

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
			Status:      BoardingSweepTxStatusFinalized,
			Txid:        swept,
			BlockHeight: 800_900,
		},
	)
	require.True(t, result.IsOk())

	// want=0 returns after the short settle window with any stray
	// emissions; a record without its tx must produce none.
	require.Empty(
		t, drain(0),
		"sweep record without tx must emit no clearing legs",
	)
}

// TestSweepTxNotificationFailedKeepsInputsReserved verifies that a terminal
// txconfirm attempt failure does not masquerade as proof that every input is
// unspent. Recovery retains the watches and schedules a fresh attempt.
func TestSweepTxNotificationFailedKeepsInputsReserved(t *testing.T) {
	t.Parallel()

	failedTxid := chainhash.Hash{0x99}
	store := &MockBoardingSweepStore{}

	// The persisted lookup is absent in this actor-only test. In production
	// the in-memory pending entry corresponds to the pending durable row.
	store.On(
		"GetBoardingSweep", mock.Anything, failedTxid,
	).Return(nil, nil)

	a := newSweepTestArk(t, store, nil, 0, 0)
	pending := &pendingSweepState{
		txid:      failedTxid,
		submitted: true,
		inputs:    map[wire.OutPoint]string{},
	}
	a.pendingSweeps[failedTxid] = pending

	result := a.handleSweepTxNotification(
		t.Context(), BoardingSweepTxNotification{
			Status: BoardingSweepTxStatusFailed,
			Txid:   failedTxid,
			Reason: "test failure",
		},
	)
	require.True(t, result.IsOk())
	require.Same(t, pending, a.pendingSweeps[failedTxid])
	require.False(t, pending.submitted)
	store.AssertNotCalled(t, "MarkBoardingSweepFailed")
	store.AssertExpectations(t)
}

// TestSweepTxNotificationReorgedDoesNotMarkFailed verifies that a
// TxReorged event from txconfirm — which arrives whenever a previously
// observed confirmation is rolled back on chain — must NOT be treated
// as a sweep failure. The handler should leave pendingSweeps and the
// persistent sweep record intact so that the next TxConfirmed on the
// new canonical chain (or, in the worst case, a chainsource spend
// notification for some other spender of the inputs) drives the
// terminal decision.
func TestSweepTxNotificationReorgedDoesNotMarkFailed(t *testing.T) {
	t.Parallel()

	reorgedTxid := chainhash.Hash{0xa1}
	store := &MockBoardingSweepStore{}
	// CRITICAL: MarkBoardingSweepFailed must NOT be called on reorg.
	// testify/mock will fail the test if any unexpected method is
	// invoked, so we simply do not register MarkBoardingSweepFailed
	// here and rely on AssertExpectations to verify the negative.

	a := newSweepTestArk(t, store, nil, 0, 0)
	pending := &pendingSweepState{
		txid:   reorgedTxid,
		inputs: map[wire.OutPoint]string{},
	}
	a.pendingSweeps[reorgedTxid] = pending

	result := a.handleSweepTxNotification(
		t.Context(), BoardingSweepTxNotification{
			Status: BoardingSweepTxStatusReorged,
			Txid:   reorgedTxid,
		},
	)
	require.True(t, result.IsOk())

	// Pending state must remain intact until Finalized commits the
	// canonical result.
	require.Same(
		t, pending, a.pendingSweeps[reorgedTxid],
		"reorg must not evict the pending sweep entry",
	)

	// Mock did not register MarkBoardingSweepFailed; AssertExpectations
	// passes vacuously, but any call would have failed the mock.
	store.AssertExpectations(t)
}

// TestSweepTxNotificationFinalizedCommitsAndCleansUp verifies that finality,
// rather than the reversible first confirmation, commits the sweep input and
// releases all in-memory tracking.
func TestSweepTxNotificationFinalizedCommitsAndCleansUp(t *testing.T) {
	t.Parallel()

	finalizedTxid := chainhash.Hash{0xa2}
	op := wire.OutPoint{Hash: chainhash.Hash{0xb2}, Index: 0}
	store := &MockBoardingSweepStore{}
	store.On(
		"MarkBoardingSweepInputSpent", mock.Anything, op,
		finalizedTxid, int32(800_750),
	).Return(true, nil)
	store.On(
		"GetBoardingSweep", mock.Anything, finalizedTxid,
	).Return(&BoardingSweepRecord{
		Txid:   finalizedTxid,
		Status: BoardingSweepStatusPublished,
		Inputs: []BoardingSweepInputRecord{{
			Txid:     finalizedTxid,
			Outpoint: op,
			Status:   BoardingSweepInputStatusPublished,
		}},
	}, nil)

	a := newSweepTestArk(t, store, nil, 0, 0)
	pending := &pendingSweepState{
		txid: finalizedTxid,
		inputs: map[wire.OutPoint]string{
			op: boardingSweepCallerID(op),
		},
	}
	a.pendingSweeps[finalizedTxid] = pending
	a.pendingSweepInputs[op] = finalizedTxid

	result := a.handleSweepTxNotification(
		t.Context(), BoardingSweepTxNotification{
			Status:      BoardingSweepTxStatusFinalized,
			Txid:        finalizedTxid,
			BlockHeight: 800_750,
			NumConfs:    31,
		},
	)
	require.True(t, result.IsOk())

	require.Empty(t, a.pendingSweeps)
	require.Empty(t, a.pendingSweepInputs)

	store.AssertExpectations(t)
}

// TestSweepTxNotificationFinalizedPersistenceFailureRearms verifies that a
// terminal chain signal cannot release watches or run later effects before all
// input transitions are durable.
func TestSweepTxNotificationFinalizedPersistenceFailureRearms(t *testing.T) {
	t.Parallel()

	finalizedTxid := chainhash.Hash{0xa9}
	op := wire.OutPoint{Hash: chainhash.Hash{0xb9}, Index: 0}
	store := &MockBoardingSweepStore{}
	store.On(
		"MarkBoardingSweepInputSpent", mock.Anything, op,
		finalizedTxid, int32(800_760),
	).Return(false, errors.New("database unavailable"))
	store.On(
		"GetBoardingSweep", mock.Anything, finalizedTxid,
	).Return(&BoardingSweepRecord{
		Txid:   finalizedTxid,
		Status: BoardingSweepStatusPublished,
		Inputs: []BoardingSweepInputRecord{{
			Txid:     finalizedTxid,
			Outpoint: op,
			Status:   BoardingSweepInputStatusPublished,
		}},
	}, nil)

	a := newSweepTestArk(t, store, nil, 0, 0)
	selfRef := actor.NewChannelTellOnlyRef[WalletMsg]("wallet-self", 1)
	a.selfRef = selfRef
	sink, drain := newCapturingLedgerSink(t)
	a.ledgerSink = fn.Some(sink)
	pending := &pendingSweepState{
		txid:      finalizedTxid,
		submitted: true,
		inputs: map[wire.OutPoint]string{
			op: boardingSweepCallerID(op),
		},
	}
	a.pendingSweeps[finalizedTxid] = pending
	a.pendingSweepInputs[op] = finalizedTxid

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	result := a.handleSweepTxNotification(
		ctx, BoardingSweepTxNotification{
			Status:      BoardingSweepTxStatusFinalized,
			Txid:        finalizedTxid,
			BlockHeight: 800_760,
			NumConfs:    31,
		},
	)
	require.True(t, result.IsOk())
	require.Same(t, pending, a.pendingSweeps[finalizedTxid])
	require.Equal(t, finalizedTxid, a.pendingSweepInputs[op])
	require.False(t, pending.submitted)

	msg, ok := selfRef.AwaitMessage(time.Second)
	require.True(
		t, ok, "failed durable finalization must schedule recovery",
	)
	_, ok = msg.(*ResumeBoardingSweepsRequest)
	require.True(t, ok)
	require.Empty(
		t, drain(0),
		"ledger must not run before all input transitions are durable",
	)
	store.AssertExpectations(t)
}

// TestSweepTxNotificationFinalizedCoversUnwatchedInputs verifies that the
// terminal backstop reads the complete durable input set instead of trusting
// the in-memory watch map. A registration failure must not leave one consumed
// boarding input available after the sweep itself reaches finality.
func TestSweepTxNotificationFinalizedCoversUnwatchedInputs(t *testing.T) {
	t.Parallel()

	finalizedTxid := chainhash.Hash{0xaa}
	armed := wire.OutPoint{Hash: chainhash.Hash{0xba}, Index: 0}
	unwatched := wire.OutPoint{Hash: chainhash.Hash{0xbb}, Index: 1}
	store := &MockBoardingSweepStore{}
	store.On(
		"GetBoardingSweep", mock.Anything, finalizedTxid,
	).Return(&BoardingSweepRecord{
		Txid:   finalizedTxid,
		Status: BoardingSweepStatusPublished,
		Inputs: []BoardingSweepInputRecord{
			{
				Txid:     finalizedTxid,
				Outpoint: armed,
				Status:   BoardingSweepInputStatusPublished,
			},
			{
				Txid:     finalizedTxid,
				Outpoint: unwatched,
				Status:   BoardingSweepInputStatusPublished,
			},
		},
	}, nil)
	store.On(
		"MarkBoardingSweepInputSpent", mock.Anything, armed,
		finalizedTxid, int32(800_770),
	).Return(false, nil)
	store.On(
		"MarkBoardingSweepInputSpent", mock.Anything, unwatched,
		finalizedTxid, int32(800_770),
	).Return(true, nil)

	a := newSweepTestArk(t, store, nil, 0, 0)
	pending := &pendingSweepState{
		txid: finalizedTxid,
		inputs: map[wire.OutPoint]string{
			armed: boardingSweepCallerID(armed),
		},
	}
	a.pendingSweeps[finalizedTxid] = pending
	a.pendingSweepInputs[armed] = finalizedTxid

	result := a.handleSweepTxNotification(
		t.Context(), BoardingSweepTxNotification{
			Status:      BoardingSweepTxStatusFinalized,
			Txid:        finalizedTxid,
			BlockHeight: 800_770,
			NumConfs:    31,
		},
	)
	require.True(t, result.IsOk())
	require.Empty(t, a.pendingSweeps)
	require.Empty(t, a.pendingSweepInputs)
	store.AssertExpectations(t)
}

// TestSweepTxNotificationConfirmedRemainsProvisional verifies that the first
// confirmation cannot mutate durable sweep success or release recovery state.
func TestSweepTxNotificationConfirmedRemainsProvisional(t *testing.T) {
	t.Parallel()

	txid := chainhash.Hash{0xa7}
	store := &MockBoardingSweepStore{}
	a := newSweepTestArk(t, store, nil, 0, 0)
	pending := &pendingSweepState{
		txid:   txid,
		inputs: map[wire.OutPoint]string{},
	}
	a.pendingSweeps[txid] = pending

	result := a.handleSweepTxNotification(
		t.Context(), BoardingSweepTxNotification{
			Status:      BoardingSweepTxStatusConfirmed,
			Txid:        txid,
			BlockHeight: 800_700,
			NumConfs:    1,
		},
	)
	require.True(t, result.IsOk())
	require.Same(t, pending, a.pendingSweeps[txid])
	store.AssertNotCalled(t, "MarkBoardingSweepInputSpent")
	store.AssertNotCalled(t, "MarkBoardingSweepFailed")
}

// TestSweepTxNotificationReorgedAfterPendingCleared verifies the
// Reorged handler arm survives a missing pendingSweeps entry (which
// can happen when every per-input spend notification has already
// resolved and the entry was cleaned up by handleSweepSpendNotification
// before the tx-level reorg notification arrives).
func TestSweepTxNotificationReorgedAfterPendingCleared(t *testing.T) {
	t.Parallel()

	reorgedTxid := chainhash.Hash{0xa3}
	store := &MockBoardingSweepStore{}
	// No store expectations — reorg with no pending entry must touch
	// nothing.

	a := newSweepTestArk(t, store, nil, 0, 0)
	// Note: pendingSweeps is intentionally empty.

	result := a.handleSweepTxNotification(
		t.Context(), BoardingSweepTxNotification{
			Status: BoardingSweepTxStatusReorged,
			Txid:   reorgedTxid,
		},
	)
	require.True(t, result.IsOk())
	require.Empty(t, a.pendingSweeps)

	store.AssertExpectations(t)
}

// TestSweepTxNotificationFailedAfterReorgedStaysReserved verifies a failed
// re-confirmation attempt after a reorg cannot restore inputs whose canonical
// spend state is still uncertain.
func TestSweepTxNotificationFailedAfterReorgedStaysReserved(t *testing.T) {
	t.Parallel()

	txid := chainhash.Hash{0xa4}
	store := &MockBoardingSweepStore{}

	store.On(
		"GetBoardingSweep", mock.Anything, txid,
	).Return(nil, nil)

	a := newSweepTestArk(t, store, nil, 0, 0)
	selfRef := actor.NewChannelTellOnlyRef[WalletMsg]("wallet-self", 1)
	a.selfRef = selfRef
	pending := &pendingSweepState{
		txid:      txid,
		submitted: true,
		inputs:    map[wire.OutPoint]string{},
	}
	a.pendingSweeps[txid] = pending

	// Step 1: Reorged — pending should remain, store should not be
	// touched.
	reorgResult := a.handleSweepTxNotification(
		t.Context(), BoardingSweepTxNotification{
			Status: BoardingSweepTxStatusReorged,
			Txid:   txid,
		},
	)
	require.True(t, reorgResult.IsOk())
	require.Same(t, pending, a.pendingSweeps[txid])

	// Step 2: Failed — the broadcaster attempt ends, but the durable
	// reservation and spend-watch ownership remain fail-closed.
	failResult := a.handleSweepTxNotification(
		t.Context(), BoardingSweepTxNotification{
			Status: BoardingSweepTxStatusFailed,
			Txid:   txid,
			Reason: "post-reorg broadcast failure",
		},
	)
	require.True(t, failResult.IsOk())
	require.Same(t, pending, a.pendingSweeps[txid])
	require.False(t, pending.submitted)
	store.AssertNotCalled(t, "MarkBoardingSweepFailed")
	store.AssertExpectations(t)
}

// TestSweepTxNotificationFailedIgnoredForConfirmedSweep verifies the
// defensive guard on the Failed arm: a spurious Failed notification for a
// sweep whose persisted record already shows a terminal-success status
// (confirmed) must NOT roll the sweep back to failed, because the
// confirmed sweep's ledger legs are irreversible and txid-keyed.
func TestSweepTxNotificationFailedIgnoredForConfirmedSweep(t *testing.T) {
	t.Parallel()

	txid := chainhash.Hash{0xa6}
	store := &MockBoardingSweepStore{}

	// The record is already confirmed, so the guard must short-circuit
	// before MarkBoardingSweepFailed; no expectation is set for it, so a
	// call would fail the test.
	store.On(
		"GetBoardingSweep", mock.Anything, txid,
	).Return(&BoardingSweepRecord{
		Status: BoardingSweepStatusConfirmed,
	}, nil)

	a := newSweepTestArk(t, store, nil, 0, 0)
	pending := &pendingSweepState{
		txid:   txid,
		inputs: map[wire.OutPoint]string{},
	}
	a.pendingSweeps[txid] = pending

	failResult := a.handleSweepTxNotification(
		t.Context(), BoardingSweepTxNotification{
			Status: BoardingSweepTxStatusFailed,
			Txid:   txid,
			Reason: "spurious failure after confirmation",
		},
	)
	require.True(t, failResult.IsOk())

	store.AssertNotCalled(t, "MarkBoardingSweepFailed")
	store.AssertExpectations(t)
}

// TestSweepTxNotificationUnknownStatusIsBenign verifies the default
// arm of handleSweepTxNotification handles an unrecognised status
// without touching the store or pendingSweeps. This guards against a
// future txconfirm lifecycle event being added without a matching
// MapNotification arm.
func TestSweepTxNotificationUnknownStatusIsBenign(t *testing.T) {
	t.Parallel()

	txid := chainhash.Hash{0xa5}
	store := &MockBoardingSweepStore{}
	// No expectations — unknown must touch nothing.

	a := newSweepTestArk(t, store, nil, 0, 0)
	pending := &pendingSweepState{
		txid:   txid,
		inputs: map[wire.OutPoint]string{},
	}
	a.pendingSweeps[txid] = pending

	result := a.handleSweepTxNotification(
		t.Context(), BoardingSweepTxNotification{
			Status: BoardingSweepTxStatusUnknown,
			Txid:   txid,
		},
	)
	require.True(t, result.IsOk())
	require.Same(t, pending, a.pendingSweeps[txid])

	store.AssertExpectations(t)
}

// TestSweepSpendNotificationReorgedDoesNotMarkSpent verifies that a
// chainsource SpendReorgedEvent on a watched boarding-sweep input is
// classified as BoardingSweepSpendStatusReorged and processed by the
// Reorged arm of handleSweepSpendNotification — specifically, the
// store's MarkBoardingSweepInputSpent must NOT be called (would
// double-spend the row's state machine) and pendingSweeps tracking
// must be left intact (the chainsource sub-actor stays alive in
// reorg-aware mode; a follow-up Spent on the canonical chain will
// drive the row through the existing happy path).
func TestSweepSpendNotificationReorgedDoesNotMarkSpent(t *testing.T) {
	t.Parallel()

	op := wire.OutPoint{Hash: chainhash.Hash{0xb1}, Index: 0}

	store := &MockBoardingSweepStore{}
	// CRITICAL: MarkBoardingSweepInputSpent must NOT be called on
	// reorg. The testify/mock will fail the test if any unexpected
	// method is invoked, so we simply do not register
	// MarkBoardingSweepInputSpent here.

	a := newSweepTestArk(t, store, nil, 0, 0)

	pendingTxid := chainhash.Hash{0xb2}
	pending := &pendingSweepState{
		txid: pendingTxid,
		inputs: map[wire.OutPoint]string{
			op: boardingSweepCallerID(op),
		},
	}
	a.pendingSweeps[pendingTxid] = pending
	a.pendingSweepInputs[op] = pendingTxid

	result := a.handleSweepSpendNotification(
		t.Context(), BoardingSweepSpendNotification{
			Status:   BoardingSweepSpendStatusReorged,
			Outpoint: op,
		},
	)
	require.True(t, result.IsOk())

	// Reorg must leave both in-memory tracking maps alone — the
	// sub-actor stays alive in reorg-aware mode and a follow-up
	// Spent event on the canonical chain will re-drive the row.
	require.Same(
		t, pending, a.pendingSweeps[pendingTxid],
		"reorg must not evict pending sweep tracking",
	)
	require.Equal(
		t, pendingTxid, a.pendingSweepInputs[op],
		"reorg must not evict per-input back-pointer",
	)
	_, provisional := pending.provisionalSpends[op]
	require.False(
		t, provisional,
		"reorg must discard the non-canonical positive observation",
	)

	store.AssertExpectations(t)
}

// TestSweepSpendNotificationDoneCommitsPositive verifies that Done seals the
// matching positive observation in the store and releases in-memory tracking.
func TestSweepSpendNotificationDoneCommitsPositive(t *testing.T) {
	t.Parallel()

	op := wire.OutPoint{Hash: chainhash.Hash{0xc1}, Index: 0}
	spendingTxid := chainhash.Hash{0xc3}

	store := &MockBoardingSweepStore{}
	store.On(
		"MarkBoardingSweepInputSpent", mock.Anything, op,
		spendingTxid, int32(140),
	).Return(true, nil)

	a := newSweepTestArk(t, store, nil, 0, 0)

	pendingTxid := chainhash.Hash{0xc2}
	pending := &pendingSweepState{
		txid: pendingTxid,
		inputs: map[wire.OutPoint]string{
			op: boardingSweepCallerID(op),
		},
		provisionalSpends: map[wire.OutPoint]provisionalSweepSpend{
			op: {
				txid:   spendingTxid,
				height: 140,
			},
		},
	}
	a.pendingSweeps[pendingTxid] = pending
	a.pendingSweepInputs[op] = pendingTxid

	result := a.handleSweepSpendNotification(
		t.Context(), BoardingSweepSpendNotification{
			Status:   BoardingSweepSpendStatusDone,
			Outpoint: op,
		},
	)
	require.True(t, result.IsOk())

	require.Empty(t, a.pendingSweeps)
	require.Empty(t, a.pendingSweepInputs)
	require.Empty(t, pending.provisionalSpends)

	store.AssertExpectations(t)
}

// TestSweepSpendNotificationPersistenceFailureRearms verifies that a final
// spend is never forgotten when the terminal store write fails. Recovery is
// owned by the wallet actor and therefore survives cancellation of the
// notification context.
func TestSweepSpendNotificationPersistenceFailureRearms(t *testing.T) {
	t.Parallel()

	op := wire.OutPoint{Hash: chainhash.Hash{0xc4}, Index: 1}
	spendingTxid := chainhash.Hash{0xc5}
	pendingTxid := chainhash.Hash{0xc6}
	store := &MockBoardingSweepStore{}
	store.On(
		"MarkBoardingSweepInputSpent", mock.Anything, op,
		spendingTxid, int32(150),
	).Return(false, errors.New("database unavailable"))

	a := newSweepTestArk(t, store, nil, 0, 0)
	selfRef := actor.NewChannelTellOnlyRef[WalletMsg]("wallet-self", 1)
	a.selfRef = selfRef
	pending := &pendingSweepState{
		txid: pendingTxid,
		inputs: map[wire.OutPoint]string{
			op: boardingSweepCallerID(op),
		},
		provisionalSpends: map[wire.OutPoint]provisionalSweepSpend{
			op: {
				txid:   spendingTxid,
				height: 150,
			},
		},
	}
	a.pendingSweeps[pendingTxid] = pending
	a.pendingSweepInputs[op] = pendingTxid

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	result := a.handleSweepSpendNotification(
		ctx, BoardingSweepSpendNotification{
			Status:   BoardingSweepSpendStatusDone,
			Outpoint: op,
		},
	)
	require.True(t, result.IsOk())
	require.Same(t, pending, a.pendingSweeps[pendingTxid])
	require.NotContains(t, pending.inputs, op)
	require.NotContains(t, a.pendingSweepInputs, op)
	require.NotContains(t, pending.provisionalSpends, op)

	msg, ok := selfRef.AwaitMessage(time.Second)
	require.True(
		t, ok, "canceled notification must still schedule recovery",
	)
	_, ok = msg.(*ResumeBoardingSweepsRequest)
	require.True(t, ok)
	store.AssertExpectations(t)
}

// TestSweepSpendNotificationDoneWithoutPositiveRearms verifies that an
// impossible Done-first ordering fails closed and reconstructs the watch from
// the still-pending durable sweep rather than inventing spender evidence.
func TestSweepSpendNotificationDoneWithoutPositiveRearms(t *testing.T) {
	t.Parallel()

	op := wire.OutPoint{Hash: chainhash.Hash{0xc7}, Index: 2}
	pendingTxid := chainhash.Hash{0xc8}
	store := &MockBoardingSweepStore{}
	a := newSweepTestArk(t, store, nil, 0, 0)
	selfRef := actor.NewChannelTellOnlyRef[WalletMsg]("wallet-self", 1)
	a.selfRef = selfRef
	pending := &pendingSweepState{
		txid: pendingTxid,
		inputs: map[wire.OutPoint]string{
			op: boardingSweepCallerID(op),
		},
	}
	a.pendingSweeps[pendingTxid] = pending
	a.pendingSweepInputs[op] = pendingTxid

	result := a.handleSweepSpendNotification(
		t.Context(), BoardingSweepSpendNotification{
			Status:   BoardingSweepSpendStatusDone,
			Outpoint: op,
		},
	)
	require.True(t, result.IsOk())
	store.AssertNotCalled(t, "MarkBoardingSweepInputSpent")

	msg, ok := selfRef.AwaitMessage(time.Second)
	require.True(t, ok)
	_, ok = msg.(*ResumeBoardingSweepsRequest)
	require.True(t, ok)
}

// TestSweepSpendNotificationUnknownStatusIsBenign verifies the default
// arm of handleSweepSpendNotification handles an unrecognised status
// without touching the store. Guards against a future chainsource
// lifecycle addition being delivered without a matching MapSpendEvent
// classification arm.
func TestSweepSpendNotificationUnknownStatusIsBenign(t *testing.T) {
	t.Parallel()

	op := wire.OutPoint{Hash: chainhash.Hash{0xd1}, Index: 0}
	store := &MockBoardingSweepStore{}
	// No expectations — unknown must touch nothing.

	a := newSweepTestArk(t, store, nil, 0, 0)
	pendingTxid := chainhash.Hash{0xd2}
	pending := &pendingSweepState{
		txid: pendingTxid,
		inputs: map[wire.OutPoint]string{
			op: boardingSweepCallerID(op),
		},
	}
	a.pendingSweeps[pendingTxid] = pending
	a.pendingSweepInputs[op] = pendingTxid

	result := a.handleSweepSpendNotification(
		t.Context(), BoardingSweepSpendNotification{
			Status:   BoardingSweepSpendStatusUnknown,
			Outpoint: op,
		},
	)
	require.True(t, result.IsOk())
	require.Same(t, pending, a.pendingSweeps[pendingTxid])

	store.AssertExpectations(t)
}
