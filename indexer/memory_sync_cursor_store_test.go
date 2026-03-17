package indexer

import (
	"context"
	"sync"
)

// MemorySyncCursorStore is a test-only in-memory SyncCursorStore
// implementation used by sync client tests.
type MemorySyncCursorStore struct {
	mu      sync.RWMutex
	cursors map[string]uint64
}

// NewMemorySyncCursorStore creates a new in-memory sync cursor store for
// tests.
func NewMemorySyncCursorStore() *MemorySyncCursorStore {
	return &MemorySyncCursorStore{
		cursors: make(map[string]uint64),
	}
}

// LoadCursor returns the stored cursor for (namespace, key).
func (s *MemorySyncCursorStore) LoadCursor(_ context.Context,
	namespace string, key string) (uint64, error) {

	s.mu.RLock()
	defer s.mu.RUnlock()

	namespacedKey := s.namespacedKey(namespace, key)

	return s.cursors[namespacedKey], nil
}

// SaveCursor stores cursor for (namespace, key) monotonically.
func (s *MemorySyncCursorStore) SaveCursor(_ context.Context,
	namespace string, key string, cursor uint64) error {

	s.mu.Lock()
	defer s.mu.Unlock()

	namespacedKey := s.namespacedKey(namespace, key)
	old := s.cursors[namespacedKey]
	if cursor < old {
		return nil
	}

	s.cursors[namespacedKey] = cursor

	return nil
}

// namespacedKey returns the canonical map key for (namespace, key).
func (s *MemorySyncCursorStore) namespacedKey(namespace string,
	key string) string {

	return namespace + "/" + key
}

var _ SyncCursorStore = (*MemorySyncCursorStore)(nil)
