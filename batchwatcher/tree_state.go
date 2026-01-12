package batchwatcher

// StateStore manages the in-memory state for all registered batches. It
// provides efficient lookup by batch ID and expiry height.
type StateStore struct {
	// batches maps batch IDs to their tree state.
	batches map[BatchID]*BatchTreeState

	// expiryIndex maps block heights to batch IDs that expire at that
	// height. This enables efficient expiry checking as blocks arrive.
	expiryIndex map[uint32][]BatchID
}

// NewStateStore creates a new empty state store.
func NewStateStore() *StateStore {
	return &StateStore{
		batches:     make(map[BatchID]*BatchTreeState),
		expiryIndex: make(map[uint32][]BatchID),
	}
}

// RegisterBatch adds a new batch to the state store. If a batch with the
// same ID already exists, it is replaced.
func (s *StateStore) RegisterBatch(state *BatchTreeState) {
	// Remove any existing entry for this batch (handles re-registration).
	s.UnregisterBatch(state.BatchID)

	// Add to batches map.
	s.batches[state.BatchID] = state

	// Add to expiry index.
	s.expiryIndex[state.ExpiryHeight] = append(
		s.expiryIndex[state.ExpiryHeight], state.BatchID,
	)
}

// UnregisterBatch removes a batch from the state store.
func (s *StateStore) UnregisterBatch(batchID BatchID) {
	state, exists := s.batches[batchID]
	if !exists {
		return
	}

	// Remove from expiry index.
	s.removeFromExpiryIndex(state.ExpiryHeight, batchID)

	// Remove from batches map.
	delete(s.batches, batchID)
}

// removeFromExpiryIndex removes a batch ID from the expiry index at the given
// height.
func (s *StateStore) removeFromExpiryIndex(height uint32, batchID BatchID) {
	batches, exists := s.expiryIndex[height]
	if !exists {
		return
	}

	// Find and remove the batch ID.
	for i, id := range batches {
		if id == batchID {
			s.expiryIndex[height] = append(
				batches[:i], batches[i+1:]...,
			)

			break
		}
	}

	// Clean up empty entries.
	if len(s.expiryIndex[height]) == 0 {
		delete(s.expiryIndex, height)
	}
}

// GetBatch returns the tree state for a batch, or nil if not found.
func (s *StateStore) GetBatch(batchID BatchID) *BatchTreeState {
	return s.batches[batchID]
}

// GetBatchesExpiringAt returns all batch IDs that expire at the given height.
func (s *StateStore) GetBatchesExpiringAt(height uint32) []BatchID {
	batches := s.expiryIndex[height]
	if batches == nil {
		return nil
	}

	// Return a copy to prevent external modification.
	result := make([]BatchID, len(batches))
	copy(result, batches)

	return result
}

// GetAllBatches returns all batch IDs currently being tracked.
func (s *StateStore) GetAllBatches() []BatchID {
	batches := make([]BatchID, 0, len(s.batches))
	for id := range s.batches {
		batches = append(batches, id)
	}

	return batches
}

// NumBatches returns the number of batches being tracked.
func (s *StateStore) NumBatches() int {
	return len(s.batches)
}
