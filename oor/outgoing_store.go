package oor

import (
	"context"
	"fmt"
)

// OutgoingSessionStore persists outgoing transfer session snapshots.
//
// This interface is intentionally minimal: v0 needs only "upsert by session id"
// and "get by session id" to support mobile-style restart/resume.
type OutgoingSessionStore interface {
	// UpsertOutgoing stores the snapshot for the session id, replacing any
	// previous snapshot.
	UpsertOutgoing(ctx context.Context, snapshot *OutgoingSnapshot) error

	// GetOutgoing returns the latest stored snapshot for the requested
	// session.
	GetOutgoing(ctx context.Context, sessionID SessionID) (
		*OutgoingSnapshot, error,
	)
}

// InMemoryOutgoingSessionStore is a process-local store implementation intended
// for unit tests.
type InMemoryOutgoingSessionStore struct {
	snapshots map[SessionID]*OutgoingSnapshot
}

// NewInMemoryOutgoingSessionStore creates a new in-memory outgoing session
// store.
func NewInMemoryOutgoingSessionStore() *InMemoryOutgoingSessionStore {
	return &InMemoryOutgoingSessionStore{
		snapshots: make(map[SessionID]*OutgoingSnapshot),
	}
}

// UpsertOutgoing stores the outgoing snapshot.
func (s *InMemoryOutgoingSessionStore) UpsertOutgoing(ctx context.Context,
	snapshot *OutgoingSnapshot) error {

	_ = ctx

	if s == nil {
		return fmt.Errorf("store must be provided")
	}

	if snapshot == nil {
		return fmt.Errorf("snapshot must be provided")
	}

	if snapshot.SessionID == (SessionID{}) {
		return fmt.Errorf("snapshot session id must be provided")
	}

	if s.snapshots == nil {
		s.snapshots = make(map[SessionID]*OutgoingSnapshot)
	}

	s.snapshots[snapshot.SessionID] = snapshot

	return nil
}

// GetOutgoing fetches the outgoing snapshot for the given session id.
func (s *InMemoryOutgoingSessionStore) GetOutgoing(ctx context.Context,
	sessionID SessionID) (*OutgoingSnapshot, error) {

	_ = ctx

	if s == nil {
		return nil, fmt.Errorf("store must be provided")
	}

	snap, ok := s.snapshots[sessionID]
	if !ok {
		return nil, fmt.Errorf("snapshot not found")
	}

	return snap, nil
}

var _ OutgoingSessionStore = (*InMemoryOutgoingSessionStore)(nil)
