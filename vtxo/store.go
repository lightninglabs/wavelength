package vtxo

import (
	"bytes"
	"context"
	"fmt"
	"sync"

	"github.com/btcsuite/btcd/wire"
)

// Status describes the lifecycle state of a VTXO.
type Status string

const (
	// StatusLive indicates the VTXO is spendable.
	StatusLive Status = "live"

	// StatusInFlight indicates the VTXO is committed to a
	// point-of-no-return operation (e.g. OOR session co-signed), but is not
	// yet finalized.
	StatusInFlight Status = "in_flight"

	// StatusSpent indicates the VTXO has been spent/finalized and is no
	// longer spendable.
	StatusSpent Status = "spent"
)

// Record is the minimal server-side record for a VTXO used in early OOR work.
//
// This is intentionally small: it captures only what the coordinator needs for
// spend gating and materializing recipient outputs in tests. The long-term
// representation will also include closure/script semantics and additional
// metadata.
type Record struct {
	// Outpoint is the unique outpoint that identifies this VTXO.
	Outpoint wire.OutPoint

	// Value is the output amount in satoshis.
	Value    int64

	// PkScript is the output script for this VTXO.
	PkScript []byte

	// Status is the current lifecycle state of this VTXO.
	Status Status

	// InFlightOwner identifies the operation holding the VTXO in-flight.
	// This is set only when Status is StatusInFlight.
	InFlightOwner LockOwner
}

// Store provides access to VTXO records and lifecycle transitions.
//
// This is the generalized server-side VTXO model that both rounds and OOR can
// eventually share. In the current iteration, it is used primarily by the OOR
// outbox driver and tests.
type Store interface {
	// Get returns the record for outpoint, or (nil, nil) if none exists.
	Get(ctx context.Context, outpoint wire.OutPoint) (*Record, error)

	// Upsert inserts or replaces a record by outpoint.
	Upsert(ctx context.Context, record *Record) error

	// MarkInFlight marks the outpoints in-flight for owner.
	//
	// The transition rules are:
	//   - live -> in_flight(owner)
	//   - in_flight(owner) -> in_flight(owner) (idempotent)
	//
	// Any other status results in an error.
	MarkInFlight(ctx context.Context, outpoints []wire.OutPoint,
		owner LockOwner) error

	// MarkSpent marks the outpoints spent.
	//
	// The transition rules are:
	//   - live -> spent
	//   - in_flight(*) -> spent
	//   - spent -> spent (idempotent)
	MarkSpent(ctx context.Context, outpoints []wire.OutPoint) error
}

// InMemoryStore is an in-memory Store implementation intended for unit tests
// and early in-process development.
type InMemoryStore struct {
	mu sync.Mutex

	records map[wire.OutPoint]*Record
}

// NewInMemoryStore creates a new empty in-memory VTXO store.
func NewInMemoryStore() *InMemoryStore {
	return &InMemoryStore{
		records: make(map[wire.OutPoint]*Record),
	}
}

// Get returns the record for outpoint, or (nil, nil) if none exists.
func (s *InMemoryStore) Get(_ context.Context,
	outpoint wire.OutPoint) (*Record, error) {

	s.mu.Lock()
	defer s.mu.Unlock()

	rec, ok := s.records[outpoint]
	if !ok {
		return nil, nil
	}

	cpy := *rec
	cpy.PkScript = bytes.Clone(rec.PkScript)

	return &cpy, nil
}

// Upsert inserts or replaces a record by outpoint.
func (s *InMemoryStore) Upsert(_ context.Context, record *Record) error {
	if record == nil {
		return fmt.Errorf("record must be provided")
	}

	if record.Value < 0 {
		return fmt.Errorf("record value must be non-negative")
	}

	if len(record.PkScript) == 0 {
		return fmt.Errorf("record pkScript must be provided")
	}

	if record.Status == "" {
		return fmt.Errorf("record status must be provided")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	cpy := *record
	cpy.PkScript = bytes.Clone(record.PkScript)
	s.records[record.Outpoint] = &cpy

	return nil
}

// MarkInFlight marks the outpoints in-flight for owner.
func (s *InMemoryStore) MarkInFlight(_ context.Context,
	outpoints []wire.OutPoint, owner LockOwner) error {

	if len(outpoints) == 0 {
		return nil
	}

	if owner == "" {
		return fmt.Errorf("owner must be provided")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Validate all state transitions before mutating any record. This
	// avoids partial updates if we discover an invalid outpoint or a
	// conflicting in-flight owner mid-loop.
	for _, op := range outpoints {
		rec, ok := s.records[op]
		if !ok {
			return fmt.Errorf("unknown vtxo: %v", op)
		}

		switch rec.Status {
		case StatusLive:
			// ok

		case StatusInFlight:
			if rec.InFlightOwner != owner {
				return fmt.Errorf("vtxo %v in-flight by %s",
					op, rec.InFlightOwner)
			}

		default:
			return fmt.Errorf("vtxo %v not spendable (%s)",
				op, rec.Status)
		}
	}

	// The set is valid. Apply the transition uniformly.
	for _, op := range outpoints {
		rec := s.records[op]
		rec.Status = StatusInFlight
		rec.InFlightOwner = owner
	}

	return nil
}

// MarkSpent marks the outpoints spent.
func (s *InMemoryStore) MarkSpent(_ context.Context,
	outpoints []wire.OutPoint) error {

	if len(outpoints) == 0 {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Validate first so the update appears atomic to callers.
	for _, op := range outpoints {
		rec, ok := s.records[op]
		if !ok {
			return fmt.Errorf("unknown vtxo: %v", op)
		}

		switch rec.Status {
		case StatusLive, StatusInFlight, StatusSpent:
			// ok

		default:
			return fmt.Errorf("unknown status %s for %v",
				rec.Status, op)
		}
	}

	// Apply the transition uniformly.
	for _, op := range outpoints {
		rec := s.records[op]
		rec.Status = StatusSpent
		rec.InFlightOwner = ""
	}

	return nil
}

var _ Store = (*InMemoryStore)(nil)
