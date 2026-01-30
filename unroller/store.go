package unroller

import (
	"context"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/round"
)

// UnrollStore provides persistence for unroll state.
type UnrollStore interface {
	// GetVTXO retrieves a VTXO by outpoint. This is used to fetch the tree
	// structure and other metadata needed for unrolling. We use
	// round.ClientVTXO directly since it contains all fields needed for
	// unrolling (TreePath and Expiry) and matches what the database layer
	// returns.
	GetVTXO(
		ctx context.Context, outpoint wire.OutPoint,
	) (*round.ClientVTXO, error)

	// SaveUnrollState creates a new unroll tracking record.
	SaveUnrollState(ctx context.Context, state *UnrollState) error

	// UpdateUnrollState updates an existing unroll record.
	UpdateUnrollState(ctx context.Context, state *UnrollState) error

	// GetUnrollState retrieves unroll state by VTXO outpoint.
	GetUnrollState(
		ctx context.Context, vtxoOutpoint wire.OutPoint,
	) (*UnrollState, error)

	// ListActiveUnrolls returns all in-progress unrolls.
	ListActiveUnrolls(ctx context.Context) ([]*UnrollState, error)

	// DeleteUnrollState removes completed unroll record.
	DeleteUnrollState(
		ctx context.Context, vtxoOutpoint wire.OutPoint,
	) error
}
