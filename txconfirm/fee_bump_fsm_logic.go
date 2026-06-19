package txconfirm

import (
	"context"
	"fmt"
)

// feeBumpStateIdle is the FSM state in which no fanout transaction is in
// flight. A fresh fanout broadcast moves the FSM into the fanout-pending
// state.
type feeBumpStateIdle struct{}

// String returns a human-readable representation of the idle state.
func (s *feeBumpStateIdle) String() string {
	return "FeeBumpIdle"
}

// IsTerminal returns false because the idle state is the FSM's resting state,
// not a terminal one: the controller keeps a single long-lived FSM and cycles
// it through fanouts for the lifetime of the actor.
func (s *feeBumpStateIdle) IsTerminal() bool {
	return false
}

// feeBumpStateSealed marks feeBumpStateIdle as a fanout state.
func (s *feeBumpStateIdle) feeBumpStateSealed() {}

// ProcessEvent applies one event to the idle fanout state. Only a fresh
// broadcast is meaningful here; confirmation, rebroadcast, and clear events
// for an already-gone fanout are tolerated as no-ops so a late chainsource
// callback never wedges the FSM.
func (s *feeBumpStateIdle) ProcessEvent(_ context.Context, event feeBumpEvent,
	_ *feeBumpEnvironment) (*feeBumpStateTransition, error) {

	switch event := event.(type) {
	case *feeBumpFanoutBroadcast:
		return &feeBumpStateTransition{
			NextState: &feeBumpStateFanoutPending{
				pending: event.pending,
			},
		}, nil

	// A confirmation, rebroadcast, or clear for a fanout we are no longer
	// tracking is a late or duplicate callback. Self-loop idle rather than
	// erroring out so a redundant chainsource event cannot tear down the
	// long-lived controller FSM.
	case *feeBumpFanoutConfirmed, *feeBumpFanoutRebroadcast,
		*feeBumpFanoutCleared:
		return &feeBumpStateTransition{
			NextState: s,
		}, nil

	default:
		return nil, fmt.Errorf("unexpected event %T in %s", event, s)
	}
}

// feeBumpStateFanoutPending is the FSM state in which a fanout transaction has
// been broadcast and is awaiting confirmation. It carries the full pending
// fanout state: txid, the funded tx, the watch script, the per-parent output
// assignments, and the last broadcast height.
type feeBumpStateFanoutPending struct {
	// pending is the in-flight fanout awaiting confirmation.
	pending *pendingFeeInputFanout
}

// String returns a human-readable representation of the fanout-pending state.
func (s *feeBumpStateFanoutPending) String() string {
	return "FeeBumpFanoutPending"
}

// IsTerminal returns false because a pending fanout is mid-lifecycle.
func (s *feeBumpStateFanoutPending) IsTerminal() bool {
	return false
}

// feeBumpStateSealed marks feeBumpStateFanoutPending as a fanout state.
func (s *feeBumpStateFanoutPending) feeBumpStateSealed() {}

// ProcessEvent applies one event to the fanout-pending state.
func (s *feeBumpStateFanoutPending) ProcessEvent(_ context.Context,
	event feeBumpEvent, _ *feeBumpEnvironment) (*feeBumpStateTransition,
	error) {

	switch event := event.(type) {
	case *feeBumpFanoutBroadcast:
		// A new fanout supersedes the one we were tracking. This can
		// happen when the previous fanout was cleared and immediately
		// rebuilt within the same controller call; carry the fresh
		// pending state forward.
		return &feeBumpStateTransition{
			NextState: &feeBumpStateFanoutPending{
				pending: event.pending,
			},
		}, nil

	case *feeBumpFanoutRebroadcast:
		// Refresh the last-broadcast height in place. Transitions must
		// be pure, so we build a fresh pending value rather than
		// mutating the one carried by the current state.
		next := *s.pending
		next.lastBroadcastHeight = event.height

		return &feeBumpStateTransition{
			NextState: &feeBumpStateFanoutPending{
				pending: &next,
			},
		}, nil

	case *feeBumpFanoutConfirmed:
		// The fanout we were tracking confirmed (the controller has
		// already promoted its outputs), so return to idle. A
		// confirmation for some other txid is ignored: self-loop the
		// current pending state.
		if event.txid != s.pending.txid {
			return &feeBumpStateTransition{
				NextState: s,
			}, nil
		}

		return &feeBumpStateTransition{
			NextState: &feeBumpStateIdle{},
		}, nil

	case *feeBumpFanoutCleared:
		// The fanout was abandoned (rejected rebroadcast or all parents
		// evicted). The controller has already released its predicted
		// outputs and leases, so return to idle.
		return &feeBumpStateTransition{
			NextState: &feeBumpStateIdle{},
		}, nil

	default:
		return nil, fmt.Errorf("unexpected event %T in %s", event, s)
	}
}
