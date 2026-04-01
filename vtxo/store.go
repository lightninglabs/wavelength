package vtxo

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
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

// Record is the server-side record for a VTXO used by OOR and round flows.
type Record struct {
	// Outpoint is the unique outpoint that identifies this VTXO.
	Outpoint wire.OutPoint

	// Value is the output amount in satoshis.
	Value int64

	// PolicyTemplate is the semantic arkscript policy for this VTXO when
	// known. The server still indexes by PkScript, but policy bytes are the
	// preferred source of ownership semantics.
	PolicyTemplate []byte

	// PkScript is the output script for this VTXO.
	PkScript []byte

	// Status is the current lifecycle state of this VTXO.
	Status Status

	// InFlightOwner identifies the operation holding the VTXO in-flight.
	// This is set only when Status is StatusInFlight.
	InFlightOwner LockOwner

	// OwnerKey is the optional owner pubkey committed to the
	// VTXO tapscript.
	OwnerKey *btcec.PublicKey

	// OperatorKeyDesc is the optional operator key descriptor
	// used for future collaborative signatures on this VTXO.
	OperatorKeyDesc *keychain.KeyDescriptor

	// ExitDelay is the optional CSV delay committed to the
	// unilateral timeout path of the VTXO tapscript.
	ExitDelay uint32
}

// Store provides access to VTXO records and lifecycle transitions.
//
// This is the generalized server-side VTXO model that both rounds and OOR can
// eventually share. In the current iteration, it is used primarily by the OOR
// outbox driver and tests.
type Store interface {
	// Get returns the record for outpoint, or (nil, nil) if none exists.
	Get(ctx context.Context, outpoint wire.OutPoint) (*Record, error)

	// Create inserts a record for outpoint.
	//
	// This is idempotent for identical records.
	// If a row already exists with conflicting fields, the call returns an
	// error.
	Create(ctx context.Context, record *Record) error

	// MarkInFlight marks the outpoints in-flight for owner.
	//
	// The transition rules are:
	//   - live -> in_flight(owner)
	//   - in_flight(owner) -> in_flight(owner) (idempotent)
	//
	// Any other status results in an error.
	MarkInFlight(ctx context.Context, outpoints []wire.OutPoint,
		owner LockOwner) error

	// MarkSpent marks outpoints spent for owner.
	//
	// The transition rules are:
	//   - in_flight(owner) -> spent
	//   - spent -> spent (idempotent for any caller
	//     because no in-flight owner remains once the
	//     record is spent)
	//
	// Any other status (including live or in_flight held by another owner)
	// returns an error.
	MarkSpent(ctx context.Context, outpoints []wire.OutPoint,
		owner LockOwner) error
}

// InMemoryStore is an in-memory Store implementation intended for unit tests
// and early in-process development.
type InMemoryStore struct {
	mu sync.Mutex

	records map[wire.OutPoint]*Record
	log     btclog.Logger
}

// ValidateUniqueOutpoints verifies the provided outpoint list has no
// duplicates.
func ValidateUniqueOutpoints(outpoints []wire.OutPoint) error {
	seen := make(map[wire.OutPoint]struct{}, len(outpoints))
	for _, op := range outpoints {
		if _, exists := seen[op]; exists {
			return fmt.Errorf("duplicate outpoint in request: "+
				"%v", op)
		}

		seen[op] = struct{}{}
	}

	return nil
}

// NewInMemoryStore creates a new empty in-memory VTXO store.
func NewInMemoryStore(
	log ...fn.Option[btclog.Logger]) *InMemoryStore {

	logger := btclog.Disabled
	if len(log) > 0 {
		logger = log[0].UnwrapOr(btclog.Disabled)
	}

	return &InMemoryStore{
		records: make(map[wire.OutPoint]*Record),
		log:     logger,
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
	cpy.PolicyTemplate = bytes.Clone(rec.PolicyTemplate)
	cpy.PkScript = bytes.Clone(rec.PkScript)

	return &cpy, nil
}

// Create inserts a record by outpoint.
//
// This is idempotent for identical records, and returns an error when a row
// already exists with conflicting fields.
func (s *InMemoryStore) Create(ctx context.Context, record *Record) error {
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
	if err := ValidateDescriptorMetadata(record); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if existing, ok := s.records[record.Outpoint]; ok {
		if existing.Value != record.Value {
			return fmt.Errorf("record %v already exists with "+
				"different value", record.Outpoint)
		}
		samePolicy := bytes.Equal(
			existing.PolicyTemplate, record.PolicyTemplate,
		)
		if !samePolicy {
			return fmt.Errorf("record %v already exists with "+
				"different policy template", record.Outpoint)
		}
		if !bytes.Equal(existing.PkScript, record.PkScript) {
			return fmt.Errorf("record %v already exists with "+
				"different pkScript", record.Outpoint)
		}
		if existing.Status != record.Status {
			return fmt.Errorf("record %v already exists with "+
				"different status %s", record.Outpoint,
				existing.Status)
		}
		if existing.InFlightOwner != record.InFlightOwner {
			return fmt.Errorf("record %v already exists with "+
				"different in-flight owner %s",
				record.Outpoint, existing.InFlightOwner)
		}
		if !samePubKey(existing.OwnerKey, record.OwnerKey) {
			return fmt.Errorf("record %v already exists with "+
				"different owner key", record.Outpoint)
		}
		if existing.ExitDelay != record.ExitDelay {
			return fmt.Errorf("record %v already exists with "+
				"different exit delay", record.Outpoint)
		}
		if !sameKeyDesc(
			existing.OperatorKeyDesc, record.OperatorKeyDesc,
		) {

			return fmt.Errorf(
				"record %v already exists with "+
					"different operator key descriptor",
				record.Outpoint,
			)
		}

		return nil
	}

	cpy := *record
	cpy.PolicyTemplate = bytes.Clone(record.PolicyTemplate)
	cpy.PkScript = bytes.Clone(record.PkScript)
	s.records[record.Outpoint] = &cpy

	s.log.DebugS(ctx, "VTXO created",
		btclog.Fmt("outpoint", "%v", record.Outpoint),
		slog.Int64("value_sat", record.Value),
		slog.String("status", string(record.Status)))

	return nil
}

// ValidateDescriptorMetadata verifies that either no collaborative descriptor
// metadata is present, or the record carries a complete consistent descriptor.
func ValidateDescriptorMetadata(record *Record) error {
	if record == nil {
		return fmt.Errorf("record must be provided")
	}

	hasMetadata := record.OwnerKey != nil ||
		record.OperatorKeyDesc != nil ||
		record.ExitDelay != 0
	if !hasMetadata {
		return nil
	}

	switch {
	case record.OwnerKey == nil:
		return fmt.Errorf("owner key must be provided")

	case record.OperatorKeyDesc == nil:
		return fmt.Errorf("operator key descriptor must be provided")

	case record.OperatorKeyDesc.PubKey == nil:
		return fmt.Errorf("operator key descriptor pubkey must be " +
			"provided")

	case record.ExitDelay == 0:
		return fmt.Errorf("exit delay must be provided")
	}

	return nil
}

// samePubKey reports whether the optional pubkeys are equal.
func samePubKey(a, b *btcec.PublicKey) bool {
	switch {
	case a == nil && b == nil:
		return true

	case a == nil || b == nil:
		return false

	default:
		return a.IsEqual(b)
	}
}

// sameKeyDesc reports whether the optional key descriptors are equal.
func sameKeyDesc(a, b *keychain.KeyDescriptor) bool {
	switch {
	case a == nil && b == nil:
		return true

	case a == nil || b == nil:
		return false

	case a.KeyLocator != b.KeyLocator:
		return false
	}

	return samePubKey(a.PubKey, b.PubKey)
}

// MarkInFlight marks the outpoints in-flight for owner.
func (s *InMemoryStore) MarkInFlight(ctx context.Context,
	outpoints []wire.OutPoint, owner LockOwner) error {

	if len(outpoints) == 0 {
		return nil
	}

	if owner == "" {
		return fmt.Errorf("owner must be provided")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	err := ValidateUniqueOutpoints(outpoints)
	if err != nil {
		return err
	}

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

	s.log.DebugS(ctx, "VTXOs marked in-flight",
		slog.Int("count", len(outpoints)),
		slog.String("owner", string(owner)))

	return nil
}

// MarkSpent marks outpoints spent for owner.
func (s *InMemoryStore) MarkSpent(ctx context.Context,
	outpoints []wire.OutPoint, owner LockOwner) error {

	if len(outpoints) == 0 {
		return nil
	}

	if owner == "" {
		return fmt.Errorf("owner must be provided")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	err := ValidateUniqueOutpoints(outpoints)
	if err != nil {
		return err
	}

	// Validate first so the update appears atomic to callers.
	for _, op := range outpoints {
		rec, ok := s.records[op]
		if !ok {
			return fmt.Errorf("unknown vtxo: %v", op)
		}

		switch rec.Status {
		case StatusInFlight:
			if rec.InFlightOwner != owner {
				return fmt.Errorf("vtxo %v in-flight by %s",
					op, rec.InFlightOwner)
			}

		case StatusSpent:
			// Already-spent rows stay idempotent regardless
			// of owner because no in-flight claim remains
			// to protect at this point.

		case StatusLive:
			return fmt.Errorf("vtxo %v not spendable (%s)",
				op, rec.Status)

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

	s.log.DebugS(ctx, "VTXOs marked spent",
		slog.Int("count", len(outpoints)))

	return nil
}

var _ Store = (*InMemoryStore)(nil)
