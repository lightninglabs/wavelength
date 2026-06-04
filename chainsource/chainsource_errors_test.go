package chainsource

import (
	"context"
	"errors"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/baselib/actor"
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

// TestActorBackendError tests that each per-request actor surfaces a backend
// registration failure synchronously from Receive. The actors' Receive methods
// return distinct fn.Result generics, so each row wraps its typed Receive call
// in a closure reporting whether it failed and the resulting error message.
func TestActorBackendError(t *testing.T) {
	t.Parallel()

	backend := &errorBackend{err: errors.New("backend error")}

	testCases := []struct {
		name        string
		register    func(ctx context.Context) (bool, string)
		expectedErr string
	}{{
		name: "conf",
		register: func(ctx context.Context) (bool, string) {
			a := NewConfActor(ConfActorConfig{Backend: backend})
			defer a.Stop()
			r := a.Receive(ctx, &RegisterConfRequest{
				CallerID:    "backend-error",
				Txid:        &chainhash.Hash{},
				PkScript:    []byte{0x00, 0x14},
				TargetConfs: 1,
			})

			return r.IsErr(), r.Err().Error()
		},
		expectedErr: "failed to register for confirmations",
	}, {
		name: "spend",
		register: func(ctx context.Context) (bool, string) {
			a := NewSpendActor(SpendActorConfig{Backend: backend})
			defer a.Stop()
			r := a.Receive(ctx, &RegisterSpendRequest{
				CallerID: "backend-error",
				Outpoint: &wire.OutPoint{},
				PkScript: []byte{0x00, 0x14},
			})

			return r.IsErr(), r.Err().Error()
		},
		expectedErr: "failed to register for spends",
	}, {
		name: "epoch",
		register: func(ctx context.Context) (bool, string) {
			a := NewBlockEpochActor(BlockEpochConfig{
				Backend: backend,
			})
			defer a.Stop()
			r := a.Receive(ctx, &SubscribeBlocksRequest{
				CallerID: "backend-error",
			})

			return r.IsErr(), r.Err().Error()
		},
		expectedErr: "failed to register for blocks",
	}}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Backend errors happen synchronously during
			// registration, so Receive returns an error
			// immediately.
			isErr, msg := tc.register(t.Context())
			require.True(t, isErr, "Receive should fail")
			require.Contains(t, msg, tc.expectedErr)
		})
	}
}

// TestActorDuplicateSubscription tests that each per-request actor rejects a
// second subscription on the same instance. Each actor serves exactly one
// subscription for resource isolation and simpler lifecycle management. The
// actors' Receive methods return distinct fn.Result generics, so each row
// wraps its typed register-twice sequence in a closure that reports whether
// the first call succeeded and the second call's error message.
func TestActorDuplicateSubscription(t *testing.T) {
	t.Parallel()

	const dupErr = "actor already has an active subscription"

	testCases := []struct {
		name      string
		subscribe func(ctx context.Context) (bool, bool, string)
	}{{
		name: "conf",
		subscribe: func(ctx context.Context) (bool, bool, string) {
			a := NewConfActor(ConfActorConfig{
				Backend: newMockBackend(),
			})
			defer a.Stop()
			req := &RegisterConfRequest{
				CallerID: "dup",
				Txid:     &chainhash.Hash{},
				PkScript: []byte{
					0x00,
					0x14,
				},
				TargetConfs: 1,
			}
			r1, r2 := a.Receive(ctx, req), a.Receive(ctx, req)

			return r1.IsOk(), r2.IsErr(), r2.Err().Error()
		},
	}, {
		name: "spend",
		subscribe: func(ctx context.Context) (bool, bool, string) {
			a := NewSpendActor(SpendActorConfig{
				Backend: newMockBackend(),
			})
			defer a.Stop()
			req := &RegisterSpendRequest{
				CallerID: "dup",
				Outpoint: &wire.OutPoint{},
				PkScript: []byte{
					0x00,
					0x14,
				},
			}
			r1, r2 := a.Receive(ctx, req), a.Receive(ctx, req)

			return r1.IsOk(), r2.IsErr(), r2.Err().Error()
		},
	}, {
		name: "epoch",
		subscribe: func(ctx context.Context) (bool, bool, string) {
			a := NewBlockEpochActor(BlockEpochConfig{
				Backend: newMockBackend(),
			})
			defer a.Stop()
			req := &SubscribeBlocksRequest{
				CallerID: "dup",
			}
			r1, r2 := a.Receive(ctx, req), a.Receive(ctx, req)

			return r1.IsOk(), r2.IsErr(), r2.Err().Error()
		},
	}}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			firstOk, secondErr, msg := tc.subscribe(t.Context())
			require.True(t, firstOk, "first subscription should ok")
			require.True(t, secondErr, "second should fail")
			require.Contains(t, msg, dupErr)
		})
	}
}
