package swaps

import (
	"context"

	loopfsm "github.com/lightninglabs/loop/fsm"
)

var (
	// receiveStateCreated is the Loop FSM key for Created.
	receiveStateCreated = receiveStateType(ReceiveStateCreated)
	// receiveStateInvoiceCreated is the Loop FSM key for InvoiceCreated.
	receiveStateInvoiceCreated = receiveStateType(
		ReceiveStateInvoiceCreated,
	)
	// receiveStateHTLCEventAccepted is the Loop FSM key for
	// HTLCEventAccepted.
	receiveStateHTLCEventAccepted = receiveStateType(
		ReceiveStateHTLCEventAccepted,
	)
	// receiveStateVHTLCFunded is the Loop FSM key for VHTLCFunded.
	receiveStateVHTLCFunded = receiveStateType(ReceiveStateVHTLCFunded)
	// receiveStateClaimInitiated is the Loop FSM key for ClaimInitiated.
	receiveStateClaimInitiated = receiveStateType(
		ReceiveStateClaimInitiated,
	)
	// receiveStateCompleted is the Loop FSM key for Completed.
	receiveStateCompleted = receiveStateType(ReceiveStateCompleted)
	// receiveStateExpired is the Loop FSM key for Expired.
	receiveStateExpired = receiveStateType(ReceiveStateExpired)
	// receiveStateNeedsIntervention is the Loop FSM key for
	// NeedsIntervention.
	receiveStateNeedsIntervention = receiveStateType(
		ReceiveStateNeedsIntervention,
	)
	// receiveStateFailed is the Loop FSM key for Failed.
	receiveStateFailed = receiveStateType(ReceiveStateFailed)
)

// receiveStateType converts a client receive state into a Loop FSM state key.
func receiveStateType(state ReceiveState) loopfsm.StateType {
	return loopfsm.StateType(state.String())
}

// receiveLoopFSM adapts the client-side Lightning-to-Ark receive flow to
// Loop's FSM package.
//
// The ReceiveSession still owns the mutable business state. The Loop FSM owns
// the transition descriptor and action dispatch, which makes intermediate
// restart/reconciliation boundaries explicit without changing the public
// blocking API.
type receiveLoopFSM struct {
	*loopfsm.StateMachine

	session *ReceiveSession
	target  ReceiveState
	runErr  error
}

// newReceiveLoopFSM builds a Loop FSM from the session's current state.
func newReceiveLoopFSM(session *ReceiveSession,
	target ReceiveState) *receiveLoopFSM {

	machine := &receiveLoopFSM{
		session: session,
		target:  target,
	}
	machine.StateMachine = loopfsm.NewStateMachineWithState(
		machine.states(), receiveLoopState(session.state), 10,
	)

	return machine
}

// advance sends one reconciliation tick through the receive FSM.
func (m *receiveLoopFSM) advance(ctx context.Context) error {
	m.runErr = nil

	err := m.SendEvent(ctx, receiveEventAdvance, nil)
	if err != nil {
		return err
	}

	return m.runErr
}

// states returns the complete receive transition descriptor used by Loop FSM.
//
// The OnAdvance self-loops are intentionally explicit: each non-terminal state
// owns one reconciliation action, while concrete transition events document the
// durable business boundary reached by that action.
func (m *receiveLoopFSM) states() loopfsm.States {
	return loopfsm.States{
		receiveStateCreated: {
			Action: m.handleCreated,
			Transitions: receiveLoopTransitionsFor(
				ReceiveStateCreated,
			),
		},
		receiveStateInvoiceCreated: {
			Action: m.handleInvoiceCreated,
			Transitions: receiveLoopTransitionsFor(
				ReceiveStateInvoiceCreated,
			),
		},
		receiveStateHTLCEventAccepted: {
			Action: m.handleHTLCEventAccepted,
			Transitions: receiveLoopTransitionsFor(
				ReceiveStateHTLCEventAccepted,
			),
		},
		receiveStateVHTLCFunded: {
			Action: m.handleVHTLCFunded,
			Transitions: receiveLoopTransitionsFor(
				ReceiveStateVHTLCFunded,
			),
		},
		receiveStateClaimInitiated: {
			Action: m.handleClaimInitiated,
			Transitions: receiveLoopTransitionsFor(
				ReceiveStateClaimInitiated,
			),
		},
		receiveStateCompleted: {
			Action: loopfsm.NoOpAction,
		},
		receiveStateExpired: {
			Action: loopfsm.NoOpAction,
		},
		receiveStateNeedsIntervention: {
			Action: loopfsm.NoOpAction,
		},
		receiveStateFailed: {
			Action: loopfsm.NoOpAction,
		},
	}
}

func receiveLoopTransitionsFor(state ReceiveState) loopfsm.Transitions {
	transitions := loopfsm.Transitions{
		receiveEventAdvance: receiveStateType(state),
	}

	for event, nextState := range receiveTransitions[state] {
		transitions[event] = receiveStateType(nextState)
	}

	return transitions
}

// handleCreated prepares the invoice and expected vHTLC script.
func (m *receiveLoopFSM) handleCreated(ctx context.Context,
	_ loopfsm.EventContext) loopfsm.EventType {

	prev := m.session.state
	if err := m.session.prepareInvoice(ctx); err != nil {
		return m.fail(ctx, err)
	}

	return m.eventForProgress(prev)
}

// handleInvoiceCreated waits until the server's HTLC mailbox event is durably
// accepted.
func (m *receiveLoopFSM) handleInvoiceCreated(ctx context.Context,
	_ loopfsm.EventContext) loopfsm.EventType {

	prev := m.session.state
	if err := m.session.waitForHTLCEvent(ctx); err != nil {
		return m.fail(ctx, err)
	}

	return m.eventForProgress(prev)
}

// handleHTLCEventAccepted waits until the accepted vHTLC is indexed as live.
func (m *receiveLoopFSM) handleHTLCEventAccepted(ctx context.Context,
	_ loopfsm.EventContext) loopfsm.EventType {

	prev := m.session.state
	if err := m.session.waitForFunding(ctx); err != nil {
		return m.fail(ctx, err)
	}

	return m.eventForProgress(prev)
}

// handleVHTLCFunded records claim intent before spending the funded vHTLC.
func (m *receiveLoopFSM) handleVHTLCFunded(ctx context.Context,
	_ loopfsm.EventContext) loopfsm.EventType {

	prev := m.session.state
	if err := m.session.mutateAndPersist(ctx, func() error {
		m.session.claimIntentRecordedInProcess = true

		return m.session.transition(receiveEventClaimInitiated)
	}); err != nil {
		return m.fail(ctx, err)
	}

	return m.eventForProgress(prev)
}

// handleClaimInitiated submits or reconciles the preimage claim spend.
func (m *receiveLoopFSM) handleClaimInitiated(ctx context.Context,
	_ loopfsm.EventContext) loopfsm.EventType {

	prev := m.session.state
	if err := m.session.claimFundedVHTLC(ctx); err != nil {
		return m.fail(ctx, err)
	}

	return m.eventForProgress(prev)
}

// eventForProgress returns the event needed to align the Loop FSM with the
// session's business state.
func (m *receiveLoopFSM) eventForProgress(
	prev ReceiveState) loopfsm.EventType {

	if m.session.state == prev || m.session.state == m.target {
		return loopfsm.NoOp
	}

	return receiveEventForState(m.session.state)
}

// fail records the original action error and moves the business state into the
// corresponding durable terminal state.
func (m *receiveLoopFSM) fail(
	ctx context.Context,
	err error,
) loopfsm.EventType {

	return handleFailure(
		ctx, err, &m.runErr,
		m.session.state == ReceiveStateExpired,
		m.session.state == ReceiveStateNeedsIntervention,
		func(ctx context.Context) error {
			return m.session.mutateAndPersist(ctx, func() error {
				return m.session.transition(receiveEventExpired)
			})
		}, receiveEventExpired,
		func(ctx context.Context, reason string) error {
			return m.session.mutateAndPersist(ctx, func() error {
				m.session.interventionReason = reason

				return m.session.transition(
					receiveEventNeedsIntervention,
				)
			})
		}, receiveEventNeedsIntervention,
		func(ctx context.Context, reason string) error {
			return m.session.mutateAndPersist(ctx, func() error {
				m.session.interventionReason = reason
				if m.session.state == ReceiveStateFailed {
					return nil
				}

				return m.session.transition(receiveEventFailed)
			})
		}, receiveEventFailed,
	)
}

// receiveLoopState maps a receive state into the Loop FSM state key.
func receiveLoopState(state ReceiveState) loopfsm.StateType {
	switch state {
	case ReceiveStateCreated:
		return receiveStateCreated
	case ReceiveStateInvoiceCreated:
		return receiveStateInvoiceCreated
	case ReceiveStateHTLCEventAccepted:
		return receiveStateHTLCEventAccepted
	case ReceiveStateVHTLCFunded:
		return receiveStateVHTLCFunded
	case ReceiveStateClaimInitiated:
		return receiveStateClaimInitiated
	case ReceiveStateCompleted:
		return receiveStateCompleted
	case ReceiveStateExpired:
		return receiveStateExpired
	case ReceiveStateNeedsIntervention:
		return receiveStateNeedsIntervention
	case ReceiveStateFailed:
		return receiveStateFailed
	default:
		return loopfsm.EmptyState
	}
}

// receiveEventForState maps a receive state to the matching Loop FSM event.
func receiveEventForState(state ReceiveState) loopfsm.EventType {
	switch state {
	case ReceiveStateInvoiceCreated:
		return receiveEventInvoiceCreated
	case ReceiveStateHTLCEventAccepted:
		return receiveEventHTLCEventAccepted
	case ReceiveStateVHTLCFunded:
		return receiveEventVHTLCFunded
	case ReceiveStateClaimInitiated:
		return receiveEventClaimInitiated
	case ReceiveStateCompleted:
		return receiveEventCompleted
	case ReceiveStateExpired:
		return receiveEventExpired
	case ReceiveStateNeedsIntervention:
		return receiveEventNeedsIntervention
	case ReceiveStateFailed:
		return receiveEventFailed
	default:
		return loopfsm.NoOp
	}
}
