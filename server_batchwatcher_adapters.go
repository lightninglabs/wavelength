package darepo

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo/batchwatcher"
	"github.com/lightninglabs/darepo/rounds"
)

// batchWatcherVTXOStore is the narrow rounds-side projection needed to adapt
// persisted VTXO and forfeit state into the batchwatcher recovery seam.
type batchWatcherVTXOStore interface {
	// GetVTXO retrieves a VTXO by outpoint.
	GetVTXO(ctx context.Context,
		outpoint wire.OutPoint) (*rounds.VTXO, error)

	// GetForfeitInfo retrieves forfeit metadata for a VTXO, if any.
	GetForfeitInfo(ctx context.Context,
		outpoint wire.OutPoint) (*rounds.ForfeitInfo, error)

	// MarkVTXOUnrolledByClient marks a live VTXO as client-unrolled.
	MarkVTXOUnrolledByClient(ctx context.Context,
		outpoint wire.OutPoint) error
}

// batchWatcherCheckpointStore is the narrow OOR-side projection needed to
// resolve a broadcastable checkpoint by spent input.
type batchWatcherCheckpointStore interface {
	// LoadCheckpointTxByInput returns the broadcastable checkpoint tx for
	// input, if one exists.
	LoadCheckpointTxByInput(ctx context.Context, input wire.OutPoint) (
		*wire.MsgTx, bool, error)
}

// batchWatcherSpendRecoveryStore adapts the rounds VTXO store to the
// batchwatcher SpendRecoveryStore interface.
type batchWatcherSpendRecoveryStore struct {
	store batchWatcherVTXOStore
}

// newBatchWatcherSpendRecoveryStore wraps store for batchwatcher recovery
// lookups.
func newBatchWatcherSpendRecoveryStore(
	store batchWatcherVTXOStore,
) batchwatcher.SpendRecoveryStore {

	return &batchWatcherSpendRecoveryStore{store: store}
}

// GetVTXO loads the persisted VTXO state for outpoint.
func (s *batchWatcherSpendRecoveryStore) GetVTXO(ctx context.Context,
	outpoint wire.OutPoint) (*batchwatcher.RecoveryVTXO, error) {

	vtxo, err := s.store.GetVTXO(ctx, outpoint)
	if err != nil {
		return nil, fmt.Errorf("get vtxo: %w", err)
	}
	if vtxo == nil {
		return nil, nil
	}

	return &batchwatcher.RecoveryVTXO{
		Outpoint: vtxo.Outpoint,
		Status: batchwatcher.VTXOStatus(
			vtxo.Status,
		),
	}, nil
}

// GetForfeitInfo loads persisted forfeit metadata for outpoint.
func (s *batchWatcherSpendRecoveryStore) GetForfeitInfo(ctx context.Context,
	outpoint wire.OutPoint) (*batchwatcher.RecoveryForfeitInfo, error) {

	info, err := s.store.GetForfeitInfo(ctx, outpoint)
	if err != nil {
		return nil, fmt.Errorf("get forfeit info: %w", err)
	}
	if info == nil {
		return nil, nil
	}

	return &batchwatcher.RecoveryForfeitInfo{
		ForfeitTx: info.ForfeitTx,
	}, nil
}

// MarkVTXOUnrolledByClient marks a live VTXO as no longer usable for
// collaborative recovery paths.
func (s *batchWatcherSpendRecoveryStore) MarkVTXOUnrolledByClient(
	ctx context.Context, outpoint wire.OutPoint) error {

	return s.store.MarkVTXOUnrolledByClient(ctx, outpoint)
}

// batchWatcherOORCheckpointLookup adapts the OOR session store to the
// batchwatcher CheckpointLookup interface.
type batchWatcherOORCheckpointLookup struct {
	store batchWatcherCheckpointStore
}

// newBatchWatcherCheckpointLookup wraps store for batchwatcher OOR checkpoint
// resolution.
func newBatchWatcherCheckpointLookup(
	store batchWatcherCheckpointStore,
) batchwatcher.CheckpointLookup {

	return &batchWatcherOORCheckpointLookup{store: store}
}

// LoadCheckpointTxByInput returns the broadcastable checkpoint tx for input,
// if one exists.
func (l *batchWatcherOORCheckpointLookup) LoadCheckpointTxByInput(
	ctx context.Context, input wire.OutPoint) (*wire.MsgTx, bool, error) {

	tx, found, err := l.store.LoadCheckpointTxByInput(ctx, input)
	if err != nil {
		return nil, false, fmt.Errorf("load checkpoint tx by input: %w",
			err)
	}

	return tx, found, nil
}
