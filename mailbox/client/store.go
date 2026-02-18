package mailboxclient

import (
	"context"
	"sync"
)

// Store persists state needed for crash-safe RPC-over-mailbox operation.
//
// Callers that want crash safety should use a durable Store implementation and
// ensure correlation IDs are stable across retries (for example, by reusing the
// RPC idempotency key as the correlation id).
type Store interface {
	// LoadCursor returns the persisted Pull cursor for mailboxID.
	LoadCursor(ctx context.Context, mailboxID string) (uint64, error)

	// SaveCursor persists the Pull cursor for mailboxID.
	//
	// Implementations SHOULD treat cursor as monotonic and MUST NOT move it
	// backward.
	SaveCursor(ctx context.Context, mailboxID string, cursor uint64) error

	// PutResponse records a response payload for correlationID.
	//
	// payload is the raw protobuf message bytes stored in an Any.Value.
	//
	// PutResponse MUST be idempotent for the same mailboxID and
	// correlationID.
	//
	// It SHOULD keep the first successfully stored payload.
	PutResponse(ctx context.Context, mailboxID string, correlationID string,
		payload []byte) error

	// GetResponse returns a previously recorded response payload.
	GetResponse(ctx context.Context, mailboxID string,
		correlationID string) (payload []byte, ok bool, err error)

	// DeleteResponse removes a previously recorded response payload.
	DeleteResponse(ctx context.Context, mailboxID string,
		correlationID string) error
}

// MemoryStore is an in-memory Store implementation.
//
// It is useful for tests and short-lived processes, but it is not crash-safe.
type MemoryStore struct {
	mu sync.Mutex

	cursors   map[string]uint64
	responses map[string]map[string][]byte
}

// NewMemoryStore constructs an empty in-memory store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		cursors:   make(map[string]uint64),
		responses: make(map[string]map[string][]byte),
	}
}

// LoadCursor returns the saved cursor for mailboxID.
func (s *MemoryStore) LoadCursor(ctx context.Context, mailboxID string) (
	uint64, error) {

	_ = ctx

	s.mu.Lock()
	defer s.mu.Unlock()

	return s.cursors[mailboxID], nil
}

// SaveCursor stores cursor for mailboxID.
func (s *MemoryStore) SaveCursor(ctx context.Context, mailboxID string,
	cursor uint64) error {

	_ = ctx

	s.mu.Lock()
	defer s.mu.Unlock()

	old := s.cursors[mailboxID]
	if cursor < old {
		return nil
	}

	s.cursors[mailboxID] = cursor

	return nil
}

// PutResponse stores payload for correlationID if it doesn't already exist.
func (s *MemoryStore) PutResponse(ctx context.Context, mailboxID string,
	correlationID string, payload []byte) error {

	_ = ctx

	s.mu.Lock()
	defer s.mu.Unlock()

	byMailbox, ok := s.responses[mailboxID]
	if !ok {
		byMailbox = make(map[string][]byte)
		s.responses[mailboxID] = byMailbox
	}

	if _, exists := byMailbox[correlationID]; exists {
		return nil
	}

	payloadCopy := make([]byte, len(payload))
	copy(payloadCopy, payload)

	byMailbox[correlationID] = payloadCopy

	return nil
}

// GetResponse returns payload for correlationID if present.
func (s *MemoryStore) GetResponse(ctx context.Context, mailboxID string,
	correlationID string) ([]byte, bool, error) {

	_ = ctx

	s.mu.Lock()
	defer s.mu.Unlock()

	byMailbox, ok := s.responses[mailboxID]
	if !ok {
		return nil, false, nil
	}

	payload, ok := byMailbox[correlationID]
	if !ok {
		return nil, false, nil
	}

	payloadCopy := make([]byte, len(payload))
	copy(payloadCopy, payload)

	return payloadCopy, true, nil
}

// DeleteResponse removes payload for correlationID if present.
func (s *MemoryStore) DeleteResponse(ctx context.Context, mailboxID string,
	correlationID string) error {

	_ = ctx

	s.mu.Lock()
	defer s.mu.Unlock()

	byMailbox, ok := s.responses[mailboxID]
	if !ok {
		return nil
	}

	delete(byMailbox, correlationID)
	if len(byMailbox) == 0 {
		delete(s.responses, mailboxID)
	}

	return nil
}
