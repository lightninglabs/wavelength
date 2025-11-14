package stores

import (
	"fmt"
	"sync"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/ark/lib/types"
)

// InMemoryVTXOStore implements VTXOStore interface using an in-memory map
type InMemoryVTXOStore struct {
	mu    sync.RWMutex
	vtxos map[string]*types.ServerVTXO // key: outpoint.String()
}

// NewInMemoryVTXOStore creates a new in-memory VTXO store
func NewInMemoryVTXOStore() VTXOStore {
	return &InMemoryVTXOStore{
		vtxos: make(map[string]*types.ServerVTXO),
	}
}

// GetVTXO retrieves a VTXO by its outpoint
func (s *InMemoryVTXOStore) GetVTXO(outpoint *wire.OutPoint) (*types.ServerVTXO, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	vtxo, exists := s.vtxos[outpoint.String()]
	if !exists {
		return nil, fmt.Errorf("VTXO not found: %s", outpoint.String())
	}

	return vtxo, nil
}

// AddVTXOs adds new VTXOs to the store
func (s *InMemoryVTXOStore) AddVTXOs(vtxos []*types.ServerVTXO) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, vtxo := range vtxos {
		key := vtxo.Outpoint.String()
		if _, exists := s.vtxos[key]; exists {
			return fmt.Errorf("VTXO already exists: %s", key)
		}
		s.vtxos[key] = vtxo
	}

	return nil
}

// RemoveVTXOs removes VTXOs from the store
func (s *InMemoryVTXOStore) RemoveVTXOs(outpoints []*wire.OutPoint) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, outpoint := range outpoints {
		key := outpoint.String()
		if _, exists := s.vtxos[key]; !exists {
			return fmt.Errorf("VTXO not found for removal: %s", key)
		}
		delete(s.vtxos, key)
	}

	return nil
}

// ListVTXOs returns all VTXOs currently stored
func (s *InMemoryVTXOStore) ListVTXOs() ([]*types.ServerVTXO, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	vtxos := make([]*types.ServerVTXO, 0, len(s.vtxos))
	for _, vtxo := range s.vtxos {
		vtxos = append(vtxos, vtxo)
	}

	return vtxos, nil
}

// Ensure InMemoryVTXOStore implements VTXOStore interface
var _ VTXOStore = (*InMemoryVTXOStore)(nil)
