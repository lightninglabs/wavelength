package oor

import (
	"context"
	"fmt"
	"sync"

	"github.com/btcsuite/btcd/btcutil/psbt"
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

	arkBySession map[SessionID]*psbt.Packet

	seen []string
}

// NewInProcessOutboxDriver creates a new in-process outbox driver.
func NewInProcessOutboxDriver() *InProcessOutboxDriver {
	return &InProcessOutboxDriver{
		arkBySession: make(map[SessionID]*psbt.Packet),
	}
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

	// Track the outbox types we executed so tests can assert that specific
	// transitions emitted the expected side effects in the expected order.
	d.mu.Lock()
	d.seen = append(d.seen, outbox.OutboxType())
	d.mu.Unlock()

	switch msg := outbox.(type) {
	case *LockInputsReq:
		return []Event{&InputsLockedEvent{}}, nil

	case *ValidateSubmitReq:
		validated, err := oorlib.ValidateSubmitPackage(
			msg.ArkPSBT, msg.CheckpointPSBTs,
		)
		if err != nil {
			return []Event{&SubmitFailedEvent{
				Reason: err.Error(),
			}}, nil
		}

		d.mu.Lock()
		d.arkBySession[sessionID] = msg.ArkPSBT
		d.mu.Unlock()

		return []Event{&SubmitValidatedEvent{
			ArkTxid: validated.ArkTxid,
		}}, nil

	case *CoSignReq:
		return []Event{&OperatorSignedEvent{}}, nil

	case *ValidateFinalizeReq:
		d.mu.Lock()
		ark := d.arkBySession[sessionID]
		d.mu.Unlock()

		if ark == nil {
			return nil, fmt.Errorf(
				"missing ark psbt for session %s",
				sessionID,
			)
		}

		err := oorlib.ValidateFinalizePackage(
			ark, msg.FinalCheckpointPSBTs,
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
