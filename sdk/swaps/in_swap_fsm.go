package swaps

import (
	"context"

	loopfsm "github.com/lightninglabs/loop/fsm"
)

var (
	// payStateCreated is the Loop FSM key for Created.
	payStateCreated = payStateType(PayStateCreated)
	// payStateSwapCreated is the Loop FSM key for SwapCreated.
	payStateSwapCreated = payStateType(PayStateSwapCreated)
	// payStateFundingInitiated is the Loop FSM key for FundingInitiated.
	payStateFundingInitiated = payStateType(PayStateFundingInitiated)
	// payStateVHTLCFunded is the Loop FSM key for VHTLCFunded.
	payStateVHTLCFunded = payStateType(PayStateVHTLCFunded)
	// payStateWaitingForClaim is the Loop FSM key for WaitingForClaim.
	payStateWaitingForClaim = payStateType(PayStateWaitingForClaim)
	// payStateCompleted is the Loop FSM key for Completed.
	payStateCompleted = payStateType(PayStateCompleted)
	// payStateExpired is the Loop FSM key for Expired.
	payStateExpired = payStateType(PayStateExpired)
	// payStateRefundInitiated is the Loop FSM key for RefundInitiated.
	payStateRefundInitiated = payStateType(PayStateRefundInitiated)
	// payStateRefunded is the Loop FSM key for Refunded.
	payStateRefunded = payStateType(PayStateRefunded)
	// payStateNeedsIntervention is the Loop FSM key for NeedsIntervention.
	payStateNeedsIntervention = payStateType(PayStateNeedsIntervention)
	// payStateFailed is the Loop FSM key for Failed.
	payStateFailed = payStateType(PayStateFailed)
)

// payStateType converts a client pay state into a Loop FSM state key.
func payStateType(state PayState) loopfsm.StateType {
	return loopfsm.StateType(state.String())
}

// payLoopFSM adapts the client-side Ark-to-Lightning pay flow to Loop's FSM
// package.
//
// The paySession keeps ownership of mutable business data and external effect
// helpers. The Loop FSM owns only the transition graph and state dispatch so
// reviewers can reason about retry and idempotency boundaries in one place.
type payLoopFSM struct {
	*loopfsm.StateMachine

	session *paySession
	target  PayState
	runErr  error
}

// newPayLoopFSM builds a Loop FSM from the pay session's current state.
func newPayLoopFSM(session *paySession, target PayState) *payLoopFSM {
	machine := &payLoopFSM{
		session: session,
		target:  target,
	}
	machine.StateMachine = loopfsm.NewStateMachineWithState(
		machine.states(), payLoopState(session.state), 10,
	)

	return machine
}

// advance sends one reconciliation tick through the pay FSM.
func (m *payLoopFSM) advance(ctx context.Context) error {
	m.runErr = nil

	err := m.SendEvent(ctx, payEventAdvance, nil)
	if err != nil {
		return err
	}

	return m.runErr
}

// states returns the complete pay transition descriptor used by Loop FSM.
//
// The OnAdvance self-loops represent retryable reconciliation states, while
// concrete transition events document each business boundary reached by the
// corresponding action.
func (m *payLoopFSM) states() loopfsm.States {
	return loopfsm.States{
		payStateCreated: {
			Action:      m.handleCreated,
			Transitions: payLoopTransitionsFor(PayStateCreated),
		},
		payStateSwapCreated: {
			Action:      m.handleSwapCreated,
			Transitions: payLoopTransitionsFor(PayStateSwapCreated),
		},
		payStateFundingInitiated: {
			Action: m.handleFundingInitiated,
			Transitions: payLoopTransitionsFor(
				PayStateFundingInitiated,
			),
		},
		payStateVHTLCFunded: {
			Action:      m.handleVHTLCFunded,
			Transitions: payLoopTransitionsFor(PayStateVHTLCFunded),
		},
		payStateWaitingForClaim: {
			Action: m.handleWaitingForClaim,
			Transitions: payLoopTransitionsFor(
				PayStateWaitingForClaim,
			),
		},
		payStateRefundInitiated: {
			Action: m.handleRefundInitiated,
			Transitions: payLoopTransitionsFor(
				PayStateRefundInitiated,
			),
		},
		payStateCompleted: {
			Action: loopfsm.NoOpAction,
		},
		payStateExpired: {
			Action: loopfsm.NoOpAction,
		},
		payStateRefunded: {
			Action: loopfsm.NoOpAction,
		},
		payStateNeedsIntervention: {
			Action: loopfsm.NoOpAction,
		},
		payStateFailed: {
			Action: loopfsm.NoOpAction,
		},
	}
}

func payLoopTransitionsFor(state PayState) loopfsm.Transitions {
	transitions := loopfsm.Transitions{
		payEventAdvance: payStateType(state),
	}

	for event, nextState := range payTransitions[state] {
		transitions[event] = payStateType(nextState)
	}

	return transitions
}

// handleCreated requests in-swap parameters and derives the vHTLC script.
func (m *payLoopFSM) handleCreated(ctx context.Context,
	_ loopfsm.EventContext) loopfsm.EventType {

	prev := m.session.state
	if err := m.session.createSwap(ctx); err != nil {
		return m.fail(ctx, err)
	}

	return m.eventForProgress(prev)
}

// handleSwapCreated reconciles or submits the vHTLC funding transfer.
func (m *payLoopFSM) handleSwapCreated(ctx context.Context,
	_ loopfsm.EventContext) loopfsm.EventType {

	prev := m.session.state
	if err := m.session.fundOrAdoptVHTLC(ctx); err != nil {
		return m.fail(ctx, err)
	}

	return m.eventForProgress(prev)
}

// handleFundingInitiated waits until funding is live or the claim preimage is
// already indexed.
func (m *payLoopFSM) handleFundingInitiated(ctx context.Context,
	_ loopfsm.EventContext) loopfsm.EventType {

	prev := m.session.state
	if err := m.session.waitForFundedVHTLC(ctx); err != nil {
		return m.fail(ctx, err)
	}

	return m.eventForProgress(prev)
}

// handleVHTLCFunded records that the client should now wait for the server's
// claim preimage.
func (m *payLoopFSM) handleVHTLCFunded(ctx context.Context,
	_ loopfsm.EventContext) loopfsm.EventType {

	prev := m.session.state
	if err := m.session.mutateAndPersist(ctx, func() error {
		return m.session.transition(payEventWaitForClaim)
	}); err != nil {
		return m.fail(ctx, err)
	}

	return m.eventForProgress(prev)
}

// handleWaitingForClaim waits until the indexed claim exposes the preimage.
func (m *payLoopFSM) handleWaitingForClaim(ctx context.Context,
	_ loopfsm.EventContext) loopfsm.EventType {

	prev := m.session.state
	if err := m.session.waitForClaimPreimage(ctx); err != nil {
		return m.fail(ctx, err)
	}

	return m.eventForProgress(prev)
}

// handleRefundInitiated reconciles or submits the timeout refund spend for a
// funded pay-side vHTLC.
func (m *payLoopFSM) handleRefundInitiated(ctx context.Context,
	_ loopfsm.EventContext) loopfsm.EventType {

	prev := m.session.state
	if err := m.session.completeRefund(ctx); err != nil {
		return m.fail(ctx, err)
	}

	return m.eventForProgress(prev)
}

// eventForProgress returns the event needed to align the Loop FSM with the
// pay session's business state.
func (m *payLoopFSM) eventForProgress(prev PayState) loopfsm.EventType {
	if m.session.state == prev || m.session.state == m.target {
		return loopfsm.NoOp
	}

	return payEventForState(m.session.state)
}

// fail records the original action error and moves the business state into the
// corresponding durable terminal state.
func (m *payLoopFSM) fail(
	ctx context.Context,
	err error,
) loopfsm.EventType {

	return handleFailure(
		ctx, err, &m.runErr,
		m.session.state == PayStateExpired,
		m.session.state == PayStateNeedsIntervention,
		func(ctx context.Context) error {
			return m.session.mutateAndPersist(ctx, func() error {
				return m.session.transition(payEventExpired)
			})
		}, payEventExpired,
		func(ctx context.Context, reason string) error {
			return m.session.mutateAndPersist(ctx, func() error {
				m.session.interventionReason = reason

				return m.session.transition(
					payEventNeedsIntervention,
				)
			})
		}, payEventNeedsIntervention,
		func(ctx context.Context, reason string) error {
			return m.session.mutateAndPersist(ctx, func() error {
				m.session.interventionReason = reason
				if m.session.state == PayStateFailed {
					return nil
				}

				return m.session.transition(payEventFailed)
			})
		}, payEventFailed,
	)
}

// payLoopState maps a pay state into the Loop FSM state key.
func payLoopState(state PayState) loopfsm.StateType {
	switch state {
	case PayStateCreated:
		return payStateCreated

	case PayStateSwapCreated:
		return payStateSwapCreated

	case PayStateFundingInitiated:
		return payStateFundingInitiated

	case PayStateVHTLCFunded:
		return payStateVHTLCFunded

	case PayStateWaitingForClaim:
		return payStateWaitingForClaim

	case PayStateCompleted:
		return payStateCompleted

	case PayStateExpired:
		return payStateExpired

	case PayStateRefundInitiated:
		return payStateRefundInitiated

	case PayStateRefunded:
		return payStateRefunded

	case PayStateNeedsIntervention:
		return payStateNeedsIntervention

	case PayStateFailed:
		return payStateFailed

	default:
		return loopfsm.EmptyState
	}
}

// payEventForState maps a pay state to the matching Loop FSM event.
func payEventForState(state PayState) loopfsm.EventType {
	switch state {
	case PayStateSwapCreated:
		return payEventSwapCreated

	case PayStateFundingInitiated:
		return payEventFundingInitiated

	case PayStateVHTLCFunded:
		return payEventVHTLCFunded

	case PayStateWaitingForClaim:
		return payEventWaitForClaim

	case PayStateCompleted:
		return payEventCompleted

	case PayStateExpired:
		return payEventExpired

	case PayStateRefundInitiated:
		return payEventRefundInitiated

	case PayStateRefunded:
		return payEventRefunded

	case PayStateNeedsIntervention:
		return payEventNeedsIntervention

	case PayStateFailed:
		return payEventFailed

	default:
		return loopfsm.NoOp
	}
}
