package darepo

import (
	"context"
	"errors"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo/batchwatcher"
	"github.com/lightninglabs/darepo/rounds"
	"github.com/stretchr/testify/require"
)

// mockBatchWatcherVTXOStore is a test double for the rounds-side recovery
// store adapter seam.
type mockBatchWatcherVTXOStore struct {
	getVTXOFunc func(context.Context,
		wire.OutPoint) (*rounds.VTXO, error)
	getForfeitInfoFunc func(context.Context,
		wire.OutPoint) (*rounds.ForfeitInfo, error)
	markVTXOUnrolledByClientFn func(context.Context, wire.OutPoint) error
}

// GetVTXO returns the configured VTXO lookup result.
func (m *mockBatchWatcherVTXOStore) GetVTXO(ctx context.Context,
	outpoint wire.OutPoint) (*rounds.VTXO, error) {

	return m.getVTXOFunc(ctx, outpoint)
}

// GetForfeitInfo returns the configured forfeit lookup result.
func (m *mockBatchWatcherVTXOStore) GetForfeitInfo(ctx context.Context,
	outpoint wire.OutPoint) (*rounds.ForfeitInfo, error) {

	return m.getForfeitInfoFunc(ctx, outpoint)
}

// MarkVTXOUnrolledByClient records the configured transition call.
func (m *mockBatchWatcherVTXOStore) MarkVTXOUnrolledByClient(
	ctx context.Context, outpoint wire.OutPoint) error {

	return m.markVTXOUnrolledByClientFn(ctx, outpoint)
}

// mockBatchWatcherCheckpointStore is a test double for the OOR checkpoint
// lookup adapter seam.
type mockBatchWatcherCheckpointStore struct {
	loadFunc func(context.Context, wire.OutPoint) (*wire.MsgTx, bool, error)
}

// LoadCheckpointTxByInput returns the configured checkpoint lookup result.
func (m *mockBatchWatcherCheckpointStore) LoadCheckpointTxByInput(
	ctx context.Context, input wire.OutPoint) (*wire.MsgTx, bool, error) {

	return m.loadFunc(ctx, input)
}

// TestBatchWatcherSpendRecoveryStore verifies the rounds-to-batchwatcher
// adapter normalizes VTXO and forfeit state without importing rounds into the
// batchwatcher package.
func TestBatchWatcherSpendRecoveryStore(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	outpoint := wire.OutPoint{Index: 7}
	forfeitTx := wire.NewMsgTx(3)

	store := &mockBatchWatcherVTXOStore{
		getVTXOFunc: func(context.Context,
			wire.OutPoint) (*rounds.VTXO, error) {

			return &rounds.VTXO{
				Outpoint: outpoint,
				Descriptor: &tree.VTXODescriptor{
					Amount: btcutil.Amount(50_000),
				},
				Status: rounds.VTXOStatusForfeited,
			}, nil
		},
		getForfeitInfoFunc: func(context.Context,
			wire.OutPoint) (*rounds.ForfeitInfo, error) {

			return &rounds.ForfeitInfo{
				ForfeitTx: forfeitTx,
			}, nil
		},
		markVTXOUnrolledByClientFn: func(_ context.Context,
			got wire.OutPoint) error {

			require.Equal(t, outpoint, got)

			return nil
		},
	}

	adapter := newBatchWatcherSpendRecoveryStore(store)

	vtxo, err := adapter.GetVTXO(ctx, outpoint)
	require.NoError(t, err)
	require.NotNil(t, vtxo)
	require.Equal(t, outpoint, vtxo.Outpoint)
	require.Equal(
		t, batchwatcher.VTXOStatusForfeited, vtxo.Status,
	)

	info, err := adapter.GetForfeitInfo(ctx, outpoint)
	require.NoError(t, err)
	require.NotNil(t, info)
	require.Same(t, forfeitTx, info.ForfeitTx)

	err = adapter.MarkVTXOUnrolledByClient(ctx, outpoint)
	require.NoError(t, err)
}

// TestBatchWatcherSpendRecoveryStoreErrors verifies the adapter preserves nil
// misses and wraps underlying lookup errors with useful context.
func TestBatchWatcherSpendRecoveryStoreErrors(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	outpoint := wire.OutPoint{Index: 9}
	wantErr := errors.New("boom")

	store := &mockBatchWatcherVTXOStore{
		getVTXOFunc: func(context.Context,
			wire.OutPoint) (*rounds.VTXO, error) {

			return nil, nil
		},
		getForfeitInfoFunc: func(context.Context,
			wire.OutPoint) (*rounds.ForfeitInfo, error) {

			return nil, wantErr
		},
		markVTXOUnrolledByClientFn: func(context.Context,
			wire.OutPoint) error {

			return wantErr
		},
	}

	adapter := newBatchWatcherSpendRecoveryStore(store)

	vtxo, err := adapter.GetVTXO(ctx, outpoint)
	require.NoError(t, err)
	require.Nil(t, vtxo)

	info, err := adapter.GetForfeitInfo(ctx, outpoint)
	require.ErrorContains(t, err, "get forfeit info")
	require.ErrorIs(t, err, wantErr)
	require.Nil(t, info)

	err = adapter.MarkVTXOUnrolledByClient(ctx, outpoint)
	require.ErrorIs(t, err, wantErr)
}

// TestBatchWatcherCheckpointLookup verifies the OOR lookup adapter forwards
// the checkpoint resolution result used by future recovery handling.
func TestBatchWatcherCheckpointLookup(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	input := wire.OutPoint{Index: 3}
	checkpointTx := wire.NewMsgTx(3)

	lookup := newBatchWatcherCheckpointLookup(
		&mockBatchWatcherCheckpointStore{
			loadFunc: func(context.Context,
				wire.OutPoint) (*wire.MsgTx, bool, error) {

				return checkpointTx, true, nil
			},
		},
	)

	tx, found, err := lookup.LoadCheckpointTxByInput(ctx, input)
	require.NoError(t, err)
	require.True(t, found)
	require.Same(t, checkpointTx, tx)
}

// TestBatchWatcherCheckpointLookupErrors verifies lookup errors are wrapped
// with enough context to distinguish them from classification failures.
func TestBatchWatcherCheckpointLookupErrors(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	input := wire.OutPoint{Index: 4}
	wantErr := errors.New("boom")

	lookup := newBatchWatcherCheckpointLookup(
		&mockBatchWatcherCheckpointStore{
			loadFunc: func(context.Context,
				wire.OutPoint) (*wire.MsgTx, bool, error) {

				return nil, false, wantErr
			},
		},
	)

	tx, found, err := lookup.LoadCheckpointTxByInput(ctx, input)
	require.ErrorContains(t, err, "load checkpoint tx by input")
	require.ErrorIs(t, err, wantErr)
	require.False(t, found)
	require.Nil(t, tx)
}
