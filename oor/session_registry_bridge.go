package oor

import (
	"context"

	"github.com/btcsuite/btcd/chainhash/v2"
	clientdb "github.com/lightninglabs/darepo-client/db"
)

// SessionRegistryStore is the durable control-plane store used by the
// per-session OOR actor (to persist its snapshot inside Commit) and by the OOR
// registry (for admission, dedup, and restore). It is satisfied by
// *clientdb.OORSessionRegistryStoreDB. Because that store joins an ambient
// durable-actor transaction when one is present, a per-session actor's
// UpsertSession lands atomically with its mailbox ack, while the registry's
// own calls open their own short transactions. Terminal status is recorded by
// the snapshot upsert (the phase-to-status mapping below), not by a dedicated
// terminal write: the reaper only re-reads the row.
type SessionRegistryStore interface {
	// UpsertSession persists or updates one session's full durable state.
	UpsertSession(ctx context.Context,
		record clientdb.OORSessionRegistryRecord) error

	// GetSession loads one session's row by session id.
	GetSession(ctx context.Context,
		sessionID chainhash.Hash) (
		*clientdb.OORSessionRegistryRecord,
		error,
	)

	// LookupActiveSessionByIdempotencyKey loads the non-failed outgoing
	// session carrying the given idempotency key, if any. Failed sessions
	// never dedup a keyed retry.
	LookupActiveSessionByIdempotencyKey(ctx context.Context,
		key string) (*clientdb.OORSessionRegistryRecord, error)

	// ListNonTerminal loads every non-terminal session row for boot
	// restore.
	ListNonTerminal(ctx context.Context) (
		[]clientdb.OORSessionRegistryRecord, error)

	// ListSessions loads every session row, terminal and non-terminal
	// alike, for coarse diagnostic listings.
	ListSessions(ctx context.Context) (
		[]clientdb.OORSessionRegistryRecord,
		error,
	)
}

// Compile-time check that the production DB store satisfies the registry store
// interface used by the per-session actor and the registry.
var _ SessionRegistryStore = (*clientdb.OORSessionRegistryStoreDB)(nil)

// outgoingPhaseStatus maps an outgoing transfer phase to its coordinator-facing
// terminal status.
func outgoingPhaseStatus(phase OutgoingPhase) clientdb.OORSessionStatus {
	switch phase {
	case OutgoingPhaseCompleted:
		return clientdb.OORSessionStatusCompleted

	case OutgoingPhaseFailed:
		return clientdb.OORSessionStatusFailed

	default:
		return clientdb.OORSessionStatusPending
	}
}

// incomingPhaseStatus maps an incoming transfer phase to its coordinator-facing
// terminal status.
func incomingPhaseStatus(phase IncomingPhase) clientdb.OORSessionStatus {
	switch phase {
	case IncomingPhaseCompleted:
		return clientdb.OORSessionStatusCompleted

	case IncomingPhaseFailed:
		return clientdb.OORSessionStatusFailed

	default:
		return clientdb.OORSessionStatusPending
	}
}

// outgoingRegistryRecord builds a registry record from a live outgoing FSM
// state. The opaque resume material is the encoded OutgoingSnapshot; the
// queryable columns (phase, idempotency key, status) are projected from it.
func outgoingRegistryRecord(sessionID SessionID,
	state State) (clientdb.OORSessionRegistryRecord, error) {

	snapshot, err := NewOutgoingSnapshot(sessionID, state)
	if err != nil {
		return clientdb.OORSessionRegistryRecord{}, err
	}

	raw, err := encodeOutgoingSnapshot(snapshot)
	if err != nil {
		return clientdb.OORSessionRegistryRecord{}, err
	}

	status := outgoingPhaseStatus(snapshot.Phase)

	lastError := ""
	if status == clientdb.OORSessionStatusFailed {
		lastError = snapshot.FailReason
	}

	return clientdb.OORSessionRegistryRecord{
		SessionID:       chainhash.Hash(sessionID),
		ActorID:         ActorIDForSession(sessionID),
		Direction:       clientdb.OORSessionDirectionOutgoing,
		Phase:           string(snapshot.Phase),
		IdempotencyKey:  snapshot.IdempotencyKey,
		Status:          status,
		LastError:       lastError,
		SnapshotData:    raw,
		SnapshotVersion: int32(snapshot.Version),
	}, nil
}

// incomingRegistryRecord builds a registry record from a live incoming FSM
// state.
func incomingRegistryRecord(sessionID SessionID,
	state SessionState) (clientdb.OORSessionRegistryRecord, error) {

	snapshot, err := NewIncomingSnapshot(sessionID, state)
	if err != nil {
		return clientdb.OORSessionRegistryRecord{}, err
	}

	raw, err := encodeIncomingSnapshot(snapshot)
	if err != nil {
		return clientdb.OORSessionRegistryRecord{}, err
	}

	status := incomingPhaseStatus(snapshot.Phase)

	lastError := ""
	if status == clientdb.OORSessionStatusFailed {
		lastError = snapshot.FailReason
	}

	return clientdb.OORSessionRegistryRecord{
		SessionID:       chainhash.Hash(sessionID),
		ActorID:         ActorIDForSession(sessionID),
		Direction:       clientdb.OORSessionDirectionIncoming,
		Phase:           string(snapshot.Phase),
		Status:          status,
		LastError:       lastError,
		SnapshotData:    raw,
		SnapshotVersion: int32(snapshot.Version),
	}, nil
}

// outgoingSessionFromRecord rebuilds a live outgoing session from a registry
// record's snapshot blob.
func outgoingSessionFromRecord(ctx context.Context,
	record clientdb.OORSessionRegistryRecord) (*Session, error) {

	snapshot, err := decodeOutgoingSnapshot(record.SnapshotData)
	if err != nil {
		return nil, err
	}

	return NewSessionFromSnapshot(ctx, snapshot)
}

// incomingSessionFromRecord rebuilds a live incoming session from a registry
// record's snapshot blob, enforcing the configured receive limits during
// decode.
func incomingSessionFromRecord(ctx context.Context,
	record clientdb.OORSessionRegistryRecord,
	limits ReceiveLimits) (*ReceiveSession, error) {

	snapshot, err := decodeIncomingSnapshotWithLimits(
		record.SnapshotData, limits,
	)
	if err != nil {
		return nil, err
	}

	return NewReceiveSessionFromSnapshot(ctx, snapshot)
}
