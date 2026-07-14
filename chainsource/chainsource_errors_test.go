package chainsource

import (
	"context"
	"errors"
	"testing"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/stretchr/testify/require"
)

// TestConfActorValidationErrors tests validation error paths in ConfActor.
func TestConfActorValidationErrors(t *testing.T) {
	t.Parallel()

	backend := newMockBackend()
	ctx := t.Context()
	confActor := NewConfActor(ConfActorConfig{Backend: backend})
	defer confActor.Stop()

	testCases := []struct {
		name        string
		req         *RegisterConfRequest
		expectedErr string
	}{
		{
			name: "missing txid and pkScript",
			req: &RegisterConfRequest{
				CallerID:    "test-validation-1",
				TargetConfs: 1,
			},
			expectedErr: "either txid or pkScript must be provided",
		},
		{
			name: "zero target confirmations",
			req: &RegisterConfRequest{
				CallerID: "test-validation-2",
				Txid:     &chainhash.Hash{},
				PkScript: []byte{
					0x00,
					0x14,
				},
				TargetConfs: 0,
			},
			expectedErr: "target confirmations must be greater " +
				"than zero",
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			result := confActor.Receive(ctx, tc.req)
			require.True(t, result.IsErr())

			err := result.Err()
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.expectedErr)
		})
	}
}

// TestSpendActorValidationErrors tests validation error paths in SpendActor.
func TestSpendActorValidationErrors(t *testing.T) {
	t.Parallel()

	backend := newMockBackend()
	ctx := t.Context()
	spendActor := NewSpendActor(SpendActorConfig{Backend: backend})
	defer spendActor.Stop()

	testCases := []struct {
		name        string
		req         *RegisterSpendRequest
		expectedErr string
	}{
		{
			name: "missing outpoint and pkScript",
			req: &RegisterSpendRequest{
				CallerID: "test-spend-validation",
			},
			expectedErr: "either outpoint or pkScript must be " +
				"provided",
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			result := spendActor.Receive(ctx, tc.req)
			require.True(t, result.IsErr())

			err := result.Err()
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.expectedErr)
		})
	}
}

// errorBackend is a mock backend that always returns errors.
type errorBackend struct {
	err error
}

func (b *errorBackend) EstimateFee(ctx context.Context, targetConf uint32) (
	btcutil.Amount, error) {

	return 0, b.err
}

func (b *errorBackend) BestBlock(ctx context.Context) (int32, chainhash.Hash,
	error) {

	return 0, chainhash.Hash{}, b.err
}

func (b *errorBackend) TestMempoolAccept(_ context.Context, _ ...*wire.MsgTx) (
	[]MempoolAcceptResult, error) {

	return nil, b.err
}

func (b *errorBackend) BroadcastTx(ctx context.Context, tx *wire.MsgTx,
	label string) error {

	return b.err
}

func (b *errorBackend) SubmitPackage(ctx context.Context, parents []*wire.MsgTx,
	child *wire.MsgTx) error {

	return b.err
}

func (b *errorBackend) RegisterConf(ctx context.Context, txid *chainhash.Hash,
	pkScript []byte, numConfs uint32, heightHint uint32,
	includeBlock bool) (*ConfRegistration, error) {

	return nil, b.err
}

func (b *errorBackend) RegisterSpend(ctx context.Context,
	outpoint *wire.OutPoint, pkScript []byte, heightHint uint32) (
	*SpendRegistration, error) {

	return nil, b.err
}

func (b *errorBackend) RegisterBlocks(ctx context.Context) (*BlockRegistration,
	error) {

	return nil, b.err
}

func (b *errorBackend) Start() error {
	return b.err
}

func (b *errorBackend) Stop() error {
	return b.err
}

// TestChainSourceActorBackendErrors tests error handling when backend fails.
func TestChainSourceActorBackendErrors(t *testing.T) {
	t.Parallel()

	testErr := errors.New("backend error")
	backend := &errorBackend{err: testErr}
	system := actor.NewActorSystem()
	defer func() {
		_ = system.Shutdown(t.Context())
	}()

	chainSource := NewChainSourceActor(ChainSourceConfig{
		Backend: backend,
		System:  system,
	})
	ref := ChainSourceKey.Spawn(system, "test-chainsource", chainSource)

	ctx := t.Context()

	feeResult := ref.Ask(ctx, &FeeEstimateRequest{TargetConf: 6}).Await(ctx)
	require.True(t, feeResult.IsErr())
	require.Contains(t, feeResult.Err().Error(), "failed to estimate fee")

	heightResult := ref.Ask(ctx, &BestHeightRequest{}).Await(ctx)
	require.True(t, heightResult.IsErr())
	require.Contains(
		t, heightResult.Err().Error(),
		"failed to get best height",
	)

	tx := wire.NewMsgTx(2)
	mempoolResult := ref.Ask(
		ctx, &TestMempoolAcceptRequest{
			Txs: []*wire.MsgTx{tx},
		},
	).Await(ctx)

	require.True(t, mempoolResult.IsErr())
	require.Contains(
		t, mempoolResult.Err().Error(),
		"failed to test mempool accept",
	)

	broadcastResult := ref.Ask(
		ctx, &BroadcastTxRequest{
			Tx: tx,
		},
	).Await(ctx)
	require.True(t, broadcastResult.IsErr())
	require.Contains(
		t, broadcastResult.Err().Error(),
		"failed to broadcast transaction",
	)

	submitResult := ref.Ask(
		ctx, &SubmitPackageRequest{
			Parents: []*wire.MsgTx{tx},
			Child:   tx,
		},
	).Await(ctx)
	require.True(t, submitResult.IsErr())
	require.Contains(
		t, submitResult.Err().Error(),
		"submit package",
	)
}

// TestConfActorBackendError tests ConfActor backend error handling.
func TestConfActorBackendError(t *testing.T) {
	t.Parallel()

	testErr := errors.New("backend error")
	backend := &errorBackend{err: testErr}
	ctx := t.Context()
	confActor := NewConfActor(ConfActorConfig{Backend: backend})
	defer confActor.Stop()

	result := confActor.Receive(ctx, &RegisterConfRequest{
		CallerID:    "test-backend-error",
		Txid:        &chainhash.Hash{},
		PkScript:    []byte{0x00, 0x14},
		TargetConfs: 1,
	})

	// Backend error now happens synchronously during registration, so
	// Receive should return an error immediately.
	require.True(
		t, result.IsErr(),
		"Receive should fail with backend error",
	)
	require.Contains(
		t, result.Err().Error(),
		"failed to register for confirmations",
	)
}

// TestSpendActorBackendError tests SpendActor backend error handling.
func TestSpendActorBackendError(t *testing.T) {
	t.Parallel()

	testErr := errors.New("backend error")
	backend := &errorBackend{err: testErr}
	ctx := t.Context()
	spendActor := NewSpendActor(SpendActorConfig{Backend: backend})
	defer spendActor.Stop()

	result := spendActor.Receive(ctx, &RegisterSpendRequest{
		CallerID: "test-spend-backend-error",
		Outpoint: &wire.OutPoint{},
		PkScript: []byte{0x00, 0x14},
	})

	// Backend error now happens synchronously during registration, so
	// Receive should return an error immediately.
	require.True(
		t, result.IsErr(),
		"Receive should fail with backend error",
	)
	require.Contains(
		t, result.Err().Error(),
		"failed to register for spends",
	)
}

// TestBlockEpochActorBackendError tests BlockEpochActor backend error
// handling.
func TestBlockEpochActorBackendError(t *testing.T) {
	t.Parallel()

	testErr := errors.New("backend error")
	backend := &errorBackend{err: testErr}
	ctx := t.Context()
	epochActor := NewBlockEpochActor(BlockEpochConfig{Backend: backend})
	defer epochActor.Stop()

	result := epochActor.Receive(ctx, &SubscribeBlocksRequest{
		CallerID: "test-epoch-backend-error",
	})

	// Backend error now happens synchronously during registration, so
	// Receive should return an error immediately.
	require.True(
		t, result.IsErr(),
		"Receive should fail with backend error",
	)
	require.Contains(
		t, result.Err().Error(),
		"failed to register for blocks",
	)
}

// TestConfActorDuplicateSubscription tests that ConfActor rejects duplicate
// subscriptions on the same actor instance. Each actor is designed to handle
// exactly one subscription for resource isolation and simpler lifecycle
// management.
func TestConfActorDuplicateSubscription(t *testing.T) {
	t.Parallel()

	backend := newMockBackend()
	ctx := t.Context()
	confActor := NewConfActor(ConfActorConfig{Backend: backend})
	defer confActor.Stop()

	result1 := confActor.Receive(ctx, &RegisterConfRequest{
		CallerID:    "test-duplicate-1",
		Txid:        &chainhash.Hash{},
		PkScript:    []byte{0x00, 0x14},
		TargetConfs: 1,
	})
	require.True(t, result1.IsOk(), "first subscription should succeed")

	// Attempting a second subscription must fail to enforce single-use.
	result2 := confActor.Receive(ctx, &RegisterConfRequest{
		CallerID:    "test-duplicate-2",
		Txid:        &chainhash.Hash{},
		PkScript:    []byte{0x00, 0x14},
		TargetConfs: 1,
	})
	require.True(t, result2.IsErr(), "second subscription should fail")
	require.Contains(
		t, result2.Err().Error(),
		"actor already has an active subscription",
	)
}

// TestSpendActorDuplicateSubscription tests that SpendActor rejects duplicate
// subscriptions on the same actor instance. Each actor is designed to handle
// exactly one subscription for resource isolation and simpler lifecycle
// management.
func TestSpendActorDuplicateSubscription(t *testing.T) {
	t.Parallel()

	backend := newMockBackend()
	ctx := t.Context()
	spendActor := NewSpendActor(SpendActorConfig{Backend: backend})
	defer spendActor.Stop()

	result1 := spendActor.Receive(ctx, &RegisterSpendRequest{
		CallerID: "test-spend-duplicate-1",
		Outpoint: &wire.OutPoint{},
		PkScript: []byte{0x00, 0x14},
	})
	require.True(t, result1.IsOk(), "first subscription should succeed")

	// Attempting a second subscription must fail to enforce single-use.
	result2 := spendActor.Receive(ctx, &RegisterSpendRequest{
		CallerID: "test-spend-duplicate-2",
		Outpoint: &wire.OutPoint{},
		PkScript: []byte{0x00, 0x14},
	})
	require.True(t, result2.IsErr(), "second subscription should fail")
	require.Contains(
		t, result2.Err().Error(),
		"actor already has an active subscription",
	)
}

// TestBlockEpochActorDuplicateSubscription tests that BlockEpochActor rejects
// duplicate subscriptions on the same actor instance. Each actor is designed to
// handle exactly one subscription for resource isolation and simpler lifecycle
// management.
func TestBlockEpochActorDuplicateSubscription(t *testing.T) {
	t.Parallel()

	backend := newMockBackend()
	ctx := t.Context()
	epochActor := NewBlockEpochActor(BlockEpochConfig{Backend: backend})
	defer epochActor.Stop()

	result1 := epochActor.Receive(ctx, &SubscribeBlocksRequest{
		CallerID: "test-epoch-duplicate-1",
	})
	require.True(t, result1.IsOk(), "first subscription should succeed")

	// Attempting a second subscription must fail to enforce single-use.
	result2 := epochActor.Receive(ctx, &SubscribeBlocksRequest{
		CallerID: "test-epoch-duplicate-2",
	})
	require.True(t, result2.IsErr(), "second subscription should fail")
	require.Contains(
		t, result2.Err().Error(),
		"actor already has an active subscription",
	)
}
