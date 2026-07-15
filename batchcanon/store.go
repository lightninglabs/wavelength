package batchcanon

import (
	"context"
	"errors"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
)

// ErrBatchNotFound is returned by Store.GetBatch when no canonicality record
// exists for the requested batch txid.
var ErrBatchNotFound = errors.New("batch canonicality record not found")

// Store is the durable query/update surface for batch canonicality records.
// It is intentionally behavior-free: it persists and retrieves observations
// and reverse-dependency edges, leaving all interpretation — state
// transitions, chain watching, and admission — to the BatchCanonicalityManager
// and the later tasks of the reorg-safety epic.
//
//nolint:interfacebloat
type Store interface {
	// UpsertBatch inserts or replaces the canonicality record for a
	// batch, including its consumed inputs and dependent VTXOs. It is the
	// single entry point for first-seeing a batch and for wholesale
	// rewrites; targeted mutations use the methods below.
	UpsertBatch(ctx context.Context, record *Record) error

	// GetBatch returns the canonicality record for a batch txid. It
	// returns ErrBatchNotFound when no record exists.
	GetBatch(ctx context.Context, txid chainhash.Hash) (*Record, error)

	// ListBatchesByState returns every batch currently in the given
	// state. Used by the manager to find batches needing a particular
	// follow-up (e.g. all provisional batches to re-check for finality).
	ListBatchesByState(ctx context.Context, state State) ([]*Record, error)

	// UpdateBatchState transitions a batch to a new canonicality state
	// without touching its other fields.
	UpdateBatchState(ctx context.Context, txid chainhash.Hash,
		state State) error

	// RecordInputConflict persists the observed conflict status of one of a
	// batch's consumed inputs (a spend by a transaction other than the
	// batch itself). It exists so restart reconciliation can rebuild the
	// per-input conflict view and not transiently downgrade a persisted
	// conflict before the conflicting spend is re-observed.
	RecordInputConflict(ctx context.Context, batchTxid chainhash.Hash,
		outpoint wire.OutPoint, conflicting, conflictFinal bool) error

	// RecordConfirmation records that the batch tx is confirmed at the
	// given best-chain height and block hash. A later RecordConfirmation
	// at a different height (after a reorg) overwrites the observation so
	// the effective expiry tracks the new confirmation.
	RecordConfirmation(ctx context.Context, txid chainhash.Hash,
		height int32, block chainhash.Hash) error

	// ClearConfirmation clears the confirmation observation for a batch,
	// reflecting that its confirming block left the best chain. It does
	// not set any terminal flag: the batch may reconfirm.
	ClearConfirmation(ctx context.Context, txid chainhash.Hash) error

	// FindBatchesConsumingOutpoint returns the txids of every recorded
	// batch that consumes the given outpoint. Used to detect input
	// conflicts: two batches consuming the same outpoint are in conflict.
	FindBatchesConsumingOutpoint(ctx context.Context,
		outpoint wire.OutPoint) ([]chainhash.Hash, error)

	// AddProvisionalConsumer records a reverse-dependency edge: the given
	// VTXO outpoint has been provisionally consumed by the given consumer
	// batch. Idempotent on (consumedVTXO, consumerBatch).
	AddProvisionalConsumer(ctx context.Context, consumedVTXO wire.OutPoint,
		consumerBatch chainhash.Hash) error

	// ListProvisionalConsumersForBatch returns the VTXO outpoints that
	// the given consumer batch provisionally consumes. Used to find the
	// VTXOs to restore when the consumer batch is invalidated.
	ListProvisionalConsumersForBatch(ctx context.Context,
		consumerBatch chainhash.Hash) ([]wire.OutPoint, error)

	// DeleteProvisionalConsumersForBatch removes every reverse-dependency
	// edge for the given consumer batch, used once the batch is canonical
	// (the consumption is no longer provisional) or fully invalidated and
	// reconciled.
	DeleteProvisionalConsumersForBatch(ctx context.Context,
		consumerBatch chainhash.Hash) error
}
