package oor

import (
	"context"
	"sync"

	oorlib "github.com/lightninglabs/darepo-client/lib/tx/oor"
)

// InProcessOutboxDriver is a reusable outbox handler intended for unit tests
// and early in-process harnesses.
//
// It provides a minimal implementation of the OOR session outbox boundary:
//
//   - Locking, signing, and finalize steps are stubbed as unconditional success
//     events so the FSM can advance.
//   - Submit/finalize validation uses the shared darepo-client lib/tx/oor
//     primitives so tests exercise real v0 package rules.
//
// The purpose of this driver is to keep tests honest without requiring wallet,
// database, or VTXO store integrations yet.
type InProcessOutboxDriver struct {
	mu sync.Mutex

	seen []string
}

// NewInProcessOutboxDriver creates a new in-process outbox driver.
func NewInProcessOutboxDriver() *InProcessOutboxDriver {
	return &InProcessOutboxDriver{}
}

// SeenOutboxTypes returns a snapshot of outbox types observed so far.
func (d *InProcessOutboxDriver) SeenOutboxTypes() []string {
	d.mu.Lock()
	defer d.mu.Unlock()

	out := make([]string, len(d.seen))
	copy(out, d.seen)

	return out
}

// Handle executes the outbox request and returns follow-up events.
func (d *InProcessOutboxDriver) Handle(ctx context.Context, sessionID SessionID,
	outbox OutboxEvent) ([]Event, error) {

	_ = ctx
	_ = sessionID

	// Track the outbox types we executed so tests can assert that specific
	// transitions emitted the expected side effects in the expected order.
	d.mu.Lock()
	d.seen = append(d.seen, outbox.OutboxType())
	d.mu.Unlock()

	switch msg := outbox.(type) {
	case *LockInputsReq:
		return []Event{&InputsLockSucceededEvent{}}, nil

	case *CoSignReq:
		return []Event{&OperatorSignedEvent{}}, nil

	case *ValidateFinalizeReq:
		err := oorlib.ValidateFinalizePackage(
			msg.ArkPSBT, msg.FinalCheckpointPSBTs,
		)
		if err != nil {
			return []Event{&FinalizeFailedEvent{
				Reason: err.Error(),
			}}, nil
		}

		return []Event{&FinalizeValidatedEvent{}}, nil

	case *FinalizeReq:
		return []Event{&FinalizeSucceededEvent{}}, nil

	case *UnlockInputsReq:
		// For tests, we treat unlock as a no-op side effect. We still
		// record it so callers can assert it happened.
		return nil, nil

	default:
		// Unknown outbox types are ignored in the in-process driver.
		// Real implementations are expected to return errors for
		// unsupported requests.
		return nil, nil
	}
}

var _ OutboxHandler = (*InProcessOutboxDriver)(nil)
