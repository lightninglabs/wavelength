package db

import (
	"context"

	"github.com/btcsuite/btcd/wire"
)

// VTXOEventType describes the persisted lifecycle transition emitted by the
// server-side VTXO stores.
type VTXOEventType string

const (
	// VTXOEventTypeCreated is emitted when a VTXO record is created.
	VTXOEventTypeCreated VTXOEventType = "created"

	// VTXOEventTypeStatusChanged is emitted when a VTXO status changes but
	// remains non-terminal.
	VTXOEventTypeStatusChanged VTXOEventType = "status_changed"

	// VTXOEventTypeTerminated is emitted when a VTXO reaches a terminal
	// state.
	VTXOEventTypeTerminated VTXOEventType = "terminated"
)

// VTXOEvent contains the data needed by the indexer bridge to persist and
// fan out wallet-scoped lifecycle updates.
type VTXOEvent struct {
	// Type is the lifecycle transition type.
	Type VTXOEventType

	// Outpoint identifies the affected VTXO.
	Outpoint wire.OutPoint

	// PkScript is the VTXO's output script.
	PkScript []byte

	// Status is the resulting status persisted in the vtxos table.
	Status string
}

// VTXOEventSink consumes VTXO lifecycle events emitted by DB stores.
//
// Implementations are best-effort and should never block critical store paths.
type VTXOEventSink interface {
	// NotifyVTXOEvent handles a single VTXO lifecycle event.
	NotifyVTXOEvent(ctx context.Context, event *VTXOEvent)
}
