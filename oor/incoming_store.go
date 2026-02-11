package oor

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

var (
	// ErrIncomingSnapshotNotFound signals that no durable incoming snapshot
	// exists for a requested session id.
	ErrIncomingSnapshotNotFound = errors.New(
		"incoming session snapshot not found",
	)
)

// IsIncomingSnapshotNotFound reports whether err indicates a missing incoming
// session snapshot.
func IsIncomingSnapshotNotFound(err error) bool {
	return errors.Is(err, ErrIncomingSnapshotNotFound)
}

// IncomingSessionStore persists incoming transfer session snapshots.
type IncomingSessionStore interface {
	// UpsertIncoming stores the snapshot for the session id, replacing any
	// previous snapshot.
	UpsertIncoming(ctx context.Context, snapshot *IncomingSnapshot) error

	// GetIncoming returns the latest stored snapshot for the requested
	// session id.
	GetIncoming(ctx context.Context, sessionID SessionID) (
		*IncomingSnapshot, error,
	)
}

// InMemoryIncomingSessionStore is a test helper implementation.
type InMemoryIncomingSessionStore struct {
	mu sync.Mutex

	snapshots map[SessionID]*IncomingSnapshot
}

// NewInMemoryIncomingSessionStore creates a new in-memory incoming session
// store.
func NewInMemoryIncomingSessionStore() *InMemoryIncomingSessionStore {
	return &InMemoryIncomingSessionStore{
		snapshots: make(map[SessionID]*IncomingSnapshot),
	}
}

// UpsertIncoming stores the incoming snapshot.
func (s *InMemoryIncomingSessionStore) UpsertIncoming(ctx context.Context,
	snapshot *IncomingSnapshot) error {

	_ = ctx

	if snapshot == nil {
		return fmt.Errorf("snapshot must be provided")
	}

	if snapshot.SessionID == (SessionID{}) {
		return fmt.Errorf("snapshot session id must be provided")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.snapshots == nil {
		s.snapshots = make(map[SessionID]*IncomingSnapshot)
	}

	s.snapshots[snapshot.SessionID] = snapshot

	return nil
}

// GetIncoming fetches the incoming snapshot for the given session id.
func (s *InMemoryIncomingSessionStore) GetIncoming(ctx context.Context,
	sessionID SessionID) (*IncomingSnapshot, error) {

	_ = ctx

	if sessionID == (SessionID{}) {
		return nil, fmt.Errorf("session id must be provided")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	snap, ok := s.snapshots[sessionID]
	if !ok {
		return nil, fmt.Errorf("%w: %s",
			ErrIncomingSnapshotNotFound, sessionID,
		)
	}

	return snap, nil
}

var _ IncomingSessionStore = (*InMemoryIncomingSessionStore)(nil)
