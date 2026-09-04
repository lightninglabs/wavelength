package swaps

import (
	"context"
	"errors"

	loopfsm "github.com/lightninglabs/loop/fsm"
)

// refreshStateType converts one durable refresh state into a Loop FSM key.
func refreshStateType(state RefreshState) loopfsm.StateType {
	return loopfsm.StateType(state.String())
}

// refreshLoopFSM dispatches one idempotent reconciliation action for each
// durable boundary in the composite refresh protocol.
type refreshLoopFSM struct {
	*loopfsm.StateMachine

	session *RefreshSession
	target  RefreshState
	runErr  error
}

// newRefreshLoopFSM restores the transport FSM at the session's persisted
// business state.
func newRefreshLoopFSM(session *RefreshSession,
	target RefreshState) *refreshLoopFSM {

	machine := &refreshLoopFSM{
		session: session,
		target:  target,
	}
	machine.StateMachine = loopfsm.NewStateMachineWithState(
		machine.states(), refreshStateType(session.state), 10,
	)

	return machine
}

// advance sends one reconciliation tick through the composite FSM.
func (m *refreshLoopFSM) advance(ctx context.Context) error {
	m.runErr = nil
	if err := m.SendEvent(ctx, refreshEventAdvance, nil); err != nil {
		return err
	}

	return m.runErr
}

// states returns every non-terminal action and terminal no-op state.
func (m *refreshLoopFSM) states() loopfsm.States {
	return loopfsm.States{
		refreshStateType(RefreshStateCreated): {
			Action: m.handleCreated,
			Transitions: refreshLoopTransitionsFor(
				RefreshStateCreated,
			),
		},
		refreshStateType(RefreshStateSwapCreated): {
			Action: m.handleSwapCreated,
			Transitions: refreshLoopTransitionsFor(
				RefreshStateSwapCreated,
			),
		},
		refreshStateType(RefreshStateFundingInitiated): {
			Action: m.handleFundingInitiated,
			Transitions: refreshLoopTransitionsFor(
				RefreshStateFundingInitiated,
			),
		},
		refreshStateType(RefreshStateInputVHTLCFunded): {
			Action: m.handleInputVHTLCFunded,
			Transitions: refreshLoopTransitionsFor(
				RefreshStateInputVHTLCFunded,
			),
		},
		refreshStateType(RefreshStateOutputHTLCEventAccepted): {
			Action: m.handleOutputHTLCEventAccepted,
			Transitions: refreshLoopTransitionsFor(
				RefreshStateOutputHTLCEventAccepted,
			),
		},
		refreshStateType(RefreshStateOutputVHTLCFunded): {
			Action: m.handleOutputVHTLCFunded,
			Transitions: refreshLoopTransitionsFor(
				RefreshStateOutputVHTLCFunded,
			),
		},
		refreshStateType(RefreshStateClaimInitiated): {
			Action: m.handleClaimInitiated,
			Transitions: refreshLoopTransitionsFor(
				RefreshStateClaimInitiated,
			),
		},
		refreshStateType(RefreshStateOutputClaimed): {
			Action: m.handleOutputClaimed,
			Transitions: refreshLoopTransitionsFor(
				RefreshStateOutputClaimed,
			),
		},
		refreshStateType(RefreshStateRefundInitiated): {
			Action: m.handleRefundInitiated,
			Transitions: refreshLoopTransitionsFor(
				RefreshStateRefundInitiated,
			),
		},
		refreshStateType(RefreshStateCompleted): {
			Action: loopfsm.NoOpAction,
		},
		refreshStateType(RefreshStateExpired): {
			Action: loopfsm.NoOpAction,
		},
		refreshStateType(RefreshStateRefunded): {
			Action: loopfsm.NoOpAction,
		},
		refreshStateType(RefreshStateNeedsIntervention): {
			Action: loopfsm.NoOpAction,
		},
		refreshStateType(RefreshStateFailed): {
			Action: loopfsm.NoOpAction,
		},
	}
}

// refreshLoopTransitionsFor adds the reconciliation self-loop to the declared
// business transitions for one state.
func refreshLoopTransitionsFor(state RefreshState) loopfsm.Transitions {
	transitions := loopfsm.Transitions{
		refreshEventAdvance: refreshStateType(state),
	}
	for event, next := range refreshTransitions[state] {
		transitions[event] = refreshStateType(next)
	}

	return transitions
}

func (m *refreshLoopFSM) handleCreated(ctx context.Context,
	_ loopfsm.EventContext) loopfsm.EventType {

	return m.runAction(ctx, m.session.createSwap)
}

func (m *refreshLoopFSM) handleSwapCreated(ctx context.Context,
	_ loopfsm.EventContext) loopfsm.EventType {

	return m.runAction(ctx, m.session.initiateFunding)
}

func (m *refreshLoopFSM) handleFundingInitiated(ctx context.Context,
	_ loopfsm.EventContext) loopfsm.EventType {

	return m.runAction(ctx, m.session.fundInputVHTLC)
}

func (m *refreshLoopFSM) handleInputVHTLCFunded(ctx context.Context,
	_ loopfsm.EventContext) loopfsm.EventType {

	return m.runAction(ctx, m.session.waitForOutputEvent)
}

func (m *refreshLoopFSM) handleOutputHTLCEventAccepted(ctx context.Context,
	_ loopfsm.EventContext) loopfsm.EventType {

	return m.runAction(ctx, m.session.waitForOutputFunding)
}

func (m *refreshLoopFSM) handleOutputVHTLCFunded(ctx context.Context,
	_ loopfsm.EventContext) loopfsm.EventType {

	return m.runAction(ctx, m.session.initiateOutputClaim)
}

func (m *refreshLoopFSM) handleClaimInitiated(ctx context.Context,
	_ loopfsm.EventContext) loopfsm.EventType {

	return m.runAction(ctx, m.session.claimOutputVHTLC)
}

func (m *refreshLoopFSM) handleOutputClaimed(ctx context.Context,
	_ loopfsm.EventContext) loopfsm.EventType {

	return m.runAction(ctx, m.session.waitForInputClaim)
}

func (m *refreshLoopFSM) handleRefundInitiated(ctx context.Context,
	_ loopfsm.EventContext) loopfsm.EventType {

	return m.runAction(ctx, m.session.completeRefund)
}

// runAction aligns the Loop FSM event with the business state mutated by one
// reconciliation action.
func (m *refreshLoopFSM) runAction(ctx context.Context,
	action func(context.Context) error) loopfsm.EventType {

	previous := m.session.state
	if err := action(ctx); err != nil {
		return m.fail(ctx, err)
	}
	if m.session.state == previous || m.session.state == m.target ||
		m.session.state.IsTerminal() {
		return loopfsm.NoOp
	}

	return refreshEventForState(m.session.state)
}

// fail preserves interrupt and retryable errors in-place while mapping
// classified terminal failures onto the durable refresh states.
func (m *refreshLoopFSM) fail(ctx context.Context,
	err error) loopfsm.EventType {

	if refreshFailureNeedsIntervention(m.session.state, err) {
		err = newInterventionError(
			"refresh action failed after funding intent became "+
				"durable", err,
		)
	}

	return handleFailure(
		ctx, err, &m.runErr,
		m.session.state == RefreshStateExpired,
		m.session.state == RefreshStateNeedsIntervention,
		func(ctx context.Context) error {
			return m.session.mutateAndPersist(ctx, func() error {
				return m.session.transition(refreshEventExpired)
			})
		}, refreshEventExpired,
		func(ctx context.Context, reason string) error {
			return m.session.mutateAndPersist(ctx, func() error {
				m.session.interventionReason = reason

				return m.session.transition(
					refreshEventNeedsIntervention,
				)
			})
		}, refreshEventNeedsIntervention,
		func(ctx context.Context, reason string) error {
			return m.session.mutateAndPersist(ctx, func() error {
				m.session.interventionReason = reason

				return m.session.transition(refreshEventFailed)
			})
		}, refreshEventFailed,
	)
}

// refreshFailureNeedsIntervention prevents an unclassified implementation or
// protocol error from declaring a refresh safe after the daemon may have
// consumed the exact source VTXO. Explicit failure errors remain available
// for cases that positively prove no value was exposed, such as a rejected
// funding session.
func refreshFailureNeedsIntervention(state RefreshState, err error) bool {
	if state == RefreshStateCreated || state == RefreshStateSwapCreated ||
		err == nil || isInterruptErr(err) ||
		errors.Is(err, ErrSwapExpired) ||
		interventionReason(err) != "" || isWalletNotReadyErr(err) {
		return false
	}

	var retryableAction *retryableActionError

	return !errors.As(err, &retryableAction)
}

// refreshEventForState maps a business state back to its transport event.
func refreshEventForState(state RefreshState) loopfsm.EventType {
	switch state {
	case RefreshStateSwapCreated:
		return refreshEventSwapCreated

	case RefreshStateFundingInitiated:
		return refreshEventFundingInitiated

	case RefreshStateInputVHTLCFunded:
		return refreshEventInputVHTLCFunded

	case RefreshStateOutputHTLCEventAccepted:
		return refreshEventOutput

	case RefreshStateOutputVHTLCFunded:
		return refreshEventOutputVHTLCFunded

	case RefreshStateClaimInitiated:
		return refreshEventClaimInitiated

	case RefreshStateOutputClaimed:
		return refreshEventOutputClaimed

	case RefreshStateCompleted:
		return refreshEventCompleted

	case RefreshStateExpired:
		return refreshEventExpired

	case RefreshStateRefundInitiated:
		return refreshEventRefundInitiated

	case RefreshStateRefunded:
		return refreshEventRefunded

	case RefreshStateNeedsIntervention:
		return refreshEventNeedsIntervention

	case RefreshStateFailed:
		return refreshEventFailed

	default:
		return loopfsm.NoOp
	}
}
