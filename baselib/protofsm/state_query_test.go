package protofsm

import (
	"context"
	"testing"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// queryTestEnv is the empty environment for the state query tests. Environment
// has no methods, so nothing is needed beyond a distinct type.
type queryTestEnv struct{}

// queryTestState is a state whose transition parks until the test releases it.
// It models the real hazard: a transition that does network, signing, or
// database work while a caller elsewhere wants to read the machine's state.
type queryTestState struct {
	// entered is closed by ProcessEvent once the transition has begun, so
	// the test can query the machine at the exact moment the driver is
	// deaf.
	entered chan struct{}

	// release unblocks the transition.
	release chan struct{}
}

// ProcessEvent signals that the transition started, then parks until released.
func (s *queryTestState) ProcessEvent(ctx context.Context, _ struct{},
	_ *queryTestEnv) (*StateTransition[
	struct{},
	struct{},
	*queryTestEnv,
], error) {

	close(s.entered)

	select {
	case <-s.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	return &StateTransition[struct{}, struct{}, *queryTestEnv]{
		NextState: s,
	}, nil
}

// IsTerminal implements State.
func (s *queryTestState) IsTerminal() bool { return false }

// String implements State.
func (s *queryTestState) String() string { return "QueryTestState" }

// queryTestReporter is a no-op ErrorReporter.
type queryTestReporter struct{}

// ReportError implements ErrorReporter.
func (queryTestReporter) ReportError(error) {}

// newQueryTestMachine returns a started machine parked in a transition, plus
// the state that gates it.
func newQueryTestMachine(t *testing.T) (*StateMachine[
	struct{},
	struct{},
	*queryTestEnv,
], *queryTestState) {

	t.Helper()

	state := &queryTestState{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}

	machine := NewStateMachine(StateMachineCfg[struct{}, struct{},
		*queryTestEnv]{
		Logger:        btclog.Disabled,
		ErrorReporter: queryTestReporter{},
		InitialState:  state,
		Env:           &queryTestEnv{},
	})

	machine.Start(t.Context())
	t.Cleanup(machine.Stop)

	// Drive the machine into the parked transition.
	machine.SendEvent(t.Context(), struct{}{})

	select {
	case <-state.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("state machine never entered the transition")
	}

	return &machine, state
}

// TestCurrentStateDoesNotParkWhileMachineIsInTransition is the regression test
// for a state query with no escape. The request channel is unbuffered and
// driveMachine serves it only from its top-level select, so a query issued
// while the machine is inside a transition used to park the caller for the
// whole duration of that transition — unbounded, since a transition may do
// network or signing work. For a caller that is itself an actor's single
// receive goroutine, that park is how one slow FSM took the whole actor down.
//
// The query must come back with an error instead. Failing fast is the point:
// the callers of this are all reads that can degrade.
func TestCurrentStateDoesNotParkWhileMachineIsInTransition(t *testing.T) {
	t.Parallel()

	machine, state := newQueryTestMachine(t)

	// The default bound applies to a caller that brought no context of its
	// own, so this must come back rather than wait for the transition. The
	// query runs on its own goroutine so a regression fails the test
	// instead of hanging the package: an unbounded park has no other
	// symptom.
	queried := make(chan error, 1)
	go func() {
		_, err := machine.CurrentState()

		queried <- err
	}()

	select {
	case err := <-queried:
		require.Error(t, err)

	case <-time.After(5 * time.Second):
		t.Fatal(
			"CurrentState parked while the machine was in a " +
				"transition",
		)
	}

	// A caller with its own context gets its own bound, and gets it back as
	// its own error.
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	_, err := machine.CurrentStateWithContext(ctx)
	require.ErrorIs(t, err, context.DeadlineExceeded)

	// Once the transition finishes, the same query is answered.
	close(state.release)

	require.EventuallyWithT(t, func(c *assert.CollectT) {
		observed, err := machine.CurrentState()
		if !assert.NoError(c, err) {
			return
		}

		assert.Equal(c, "QueryTestState", observed.String())
	}, 5*time.Second, 10*time.Millisecond)
}

// TestCurrentStateWithContextHonoursCallerCancel pins that a caller which gives
// up before the machine answers gets its own cancellation back, rather than
// waiting out the default bound.
func TestCurrentStateWithContextHonoursCallerCancel(t *testing.T) {
	t.Parallel()

	machine, _ := newQueryTestMachine(t)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := machine.CurrentStateWithContext(ctx)
	require.ErrorIs(t, err, context.Canceled)
}
