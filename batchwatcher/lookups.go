package batchwatcher

import (
	"context"

	"github.com/btcsuite/btcd/wire"
)

// VTXOStatus is the subset of persisted VTXO lifecycle states that the
// batchwatcher needs for recognized client-owned spend handling.
type VTXOStatus string

const (
	// VTXOStatusPending indicates the round output has been broadcast
	// but not yet confirmed. Batchwatcher should not see on-chain
	// leaf spends in this state, but the value is preserved for
	// completeness.
	VTXOStatusPending VTXOStatus = "pending"

	// VTXOStatusLive indicates the VTXO is confirmed and still eligible for
	// collaborative OOR or forfeit handling.
	VTXOStatusLive VTXOStatus = "live"

	// VTXOStatusForfeited indicates the VTXO has already been consumed by a
	// later round's forfeit path.
	VTXOStatusForfeited VTXOStatus = "forfeited"

	// VTXOStatusUnrolledByClient indicates a recognized client-owned
	// path has already revealed the VTXO on-chain, so collaborative
	// recovery paths must no longer use it.
	VTXOStatusUnrolledByClient VTXOStatus = "unrolled_by_client"
)

// RecoveryVTXO is the minimal VTXO view batchwatcher needs when classifying a
// recognized non-branch spend of a leaf.
type RecoveryVTXO struct {
	// Outpoint identifies the VTXO.
	Outpoint wire.OutPoint

	// Status is the persisted lifecycle state for the VTXO.
	Status VTXOStatus
}

// RecoveryForfeitInfo is the minimal forfeit metadata batchwatcher needs when
// deciding whether it must recover funds through a forfeited VTXO path.
type RecoveryForfeitInfo struct {
	// ForfeitTx is the broadcastable forfeit transaction, if known.
	ForfeitTx *wire.MsgTx
}

// SpendRecoveryStore provides the batchwatcher-facing view over persisted VTXO
// and forfeit state. Keeping this projection narrow avoids a dependency cycle
// on the rounds package.
type SpendRecoveryStore interface {
	// GetVTXO loads the persisted VTXO state for outpoint. It
	// returns nil if the VTXO is unknown.
	GetVTXO(ctx context.Context, outpoint wire.OutPoint) (
		*RecoveryVTXO, error,
	)

	// GetForfeitInfo loads persisted forfeit metadata for outpoint.
	// It returns nil if the VTXO has no stored forfeit info.
	GetForfeitInfo(ctx context.Context, outpoint wire.OutPoint) (
		*RecoveryForfeitInfo, error,
	)

	// MarkVTXOUnrolledByClient marks a live VTXO as revealed by a
	// recognized client-owned spend path.
	MarkVTXOUnrolledByClient(ctx context.Context,
		outpoint wire.OutPoint) error
}

// CheckpointLookup provides the batchwatcher-facing lookup for the broadcast
// checkpoint transaction associated with an OOR-spent VTXO.
type CheckpointLookup interface {
	// LoadCheckpointTxByInput returns the broadcastable checkpoint
	// transaction that spends input, if one exists.
	LoadCheckpointTxByInput(ctx context.Context, input wire.OutPoint) (
		*wire.MsgTx, bool, error,
	)
}
