package indexer

import (
	"context"
	"time"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo/rounds"
)

// ScriptRegistrationReader is the narrow read surface used by
// RegistrationScriptAuthorizer to validate script registrations.
type ScriptRegistrationReader interface {
	// ListActiveReceiveScriptsByPrincipal returns active
	// registrations for a specific principal.
	ListActiveReceiveScriptsByPrincipal(ctx context.Context,
		principal string,
		now time.Time) ([]ReceiveScript, error)
}

// TreeLoader provides deserialized VTXO trees from storage.
//
// The concrete implementation handles round lookup, final-tx
// deserialization, sweep-key derivation, and recursive tree
// reconstruction internally.
type TreeLoader interface {
	// LoadVTXOTree returns the fully deserialized VTXO tree for a
	// given round and batch output index.
	LoadVTXOTree(ctx context.Context,
		roundID rounds.RoundID,
		batchOutputIndex int) (*tree.Tree, error)
}

// VTXOReader provides read access to VTXO rows and their associated
// round metadata.
type VTXOReader interface {
	// ListVTXOsByPkScripts returns all VTXOs matching the given
	// output scripts.
	ListVTXOsByPkScripts(ctx context.Context,
		pkScripts [][]byte) ([]VTXORow, error)

	// GetVTXO returns a single VTXO by outpoint.
	GetVTXO(ctx context.Context,
		outpoint wire.OutPoint) (VTXORow, error)

	// GetRound returns a single round by its round ID.
	GetRound(ctx context.Context,
		roundID rounds.RoundID) (RoundRow, error)

	// ListRoundsByIDs returns rounds matching the given round IDs
	// in a single batch query.
	ListRoundsByIDs(ctx context.Context,
		roundIDs []rounds.RoundID) ([]RoundRow, error)
}

// OORReader provides read access to OOR sessions, checkpoints, and
// recipient events.
type OORReader interface {
	// GetOORRecipientEventBySessionOutput returns a single OOR
	// recipient event by session and output index.
	GetOORRecipientEventBySessionOutput(ctx context.Context,
		recipientPkScript, sessionID []byte,
		outputIndex int32) (OORRecipientEvent, error)

	// GetOORSession returns an OOR session by its session ID.
	GetOORSession(ctx context.Context,
		sessionID []byte) (OORSession, error)

	// ListOORCheckpoints returns checkpoint PSBTs for a given OOR
	// session DB ID.
	ListOORCheckpoints(ctx context.Context,
		sessionDBID int32) ([]OORCheckpoint, error)
}

// Store captures the full database query surface used by the indexer
// service and its sub-components.
//
// It composes several narrower reader interfaces and adds the write
// operations needed for event persistence and script registration.
// All types used in the interface are indexer-native domain types;
// the concrete adapter translates to/from the underlying sqlc layer.
//
//nolint:interfacebloat
type Store interface {
	ScriptRegistrationReader
	TreeLoader
	VTXOReader
	OORReader

	// UpsertReceiveScript inserts or updates a receive script
	// registration for a given principal.
	UpsertReceiveScript(ctx context.Context,
		principal string, pkScript []byte,
		expiresAt time.Time, label string,
		updatedAt time.Time, ownerPubKey,
		operatorPubKey []byte, exitDelay uint32) error

	// DeleteReceiveScript removes a receive script registration.
	DeleteReceiveScript(ctx context.Context,
		principal string, pkScript []byte) (int64, error)

	// ListOORRecipientEventsAfterWithSession returns OOR recipient
	// events joined with session data, paginated by event ID.
	ListOORRecipientEventsAfterWithSession(ctx context.Context,
		recipientPkScript []byte, afterEventID int64,
		limit int32) ([]OORRecipientEventWithSession, error)

	// GetOORSessionCheckpoints returns all checkpoint PSBTs for a
	// session, ordered by index.
	GetOORSessionCheckpoints(ctx context.Context,
		sessionID []byte) ([]OORSessionCheckpoint, error)

	// ExecReadTx runs fn inside a read-only database transaction,
	// providing a transactional Store to the callback. All queries
	// issued through the callback's store see a consistent
	// snapshot. Implementations without transaction support may
	// run fn directly against the non-transactional store.
	ExecReadTx(ctx context.Context, fn func(Store) error) error

	// InsertOORRecipientEvent inserts an OOR recipient event and
	// returns the assigned row ID.
	InsertOORRecipientEvent(ctx context.Context,
		recipientPkScript []byte, eventID int64,
		sessionDBID, outputIndex int32,
		value int64,
		createdAt time.Time) (int64, error)

	// GetMaxOORRecipientEventID returns the current maximum event
	// ID for a given pkScript (used in CAS allocation).
	GetMaxOORRecipientEventID(ctx context.Context,
		recipientPkScript []byte) (int64, error)

	// ListActiveReceivePrincipalsByScript returns active
	// registrations for a given pkScript (used for event fan-out).
	ListActiveReceivePrincipalsByScript(ctx context.Context,
		pkScript []byte,
		now time.Time) ([]ReceiveScript, error)

	// ListVTXOEventsAfterByScripts returns VTXO lifecycle events
	// for the given scripts, paginated by event ID.
	ListVTXOEventsAfterByScripts(ctx context.Context,
		afterEventID int64, pkScripts [][]byte,
		limit int32) ([]VTXOEvent, error)

	// InsertVTXOEvent inserts a VTXO lifecycle event and returns
	// the assigned event ID.
	InsertVTXOEvent(ctx context.Context,
		pkScript []byte, eventType string,
		outpoint wire.OutPoint,
		vtxoStatus string,
		createdAt time.Time) (int64, error)
}
