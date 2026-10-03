package protofsm

import (
	"context"
	"testing"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// slowState is a state whose transition takes a moment, so the future for the
// event is still incomplete when the caller starts waiting on it.
type slowState struct{}

// ProcessEvent sleeps briefly and stays in the same state.
func (s *slowState) ProcessEvent(ctx context.Context, _ struct{},
	_ *queryTestEnv) (*StateTransition[
	struct{},
	struct{},
	*queryTestEnv,
], error) {

	select {
	case <-time.After(50 * time.Millisecond):
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	return &StateTransition[struct{}, struct{}, *queryTestEnv]{
		NextState: s,
	}, nil
}

// IsTerminal implements State.
func (s *slowState) IsTerminal() bool { return false }

// String implements State.
func (s *slowState) String() string { return "SlowState" }

// TestDriverFutureAwaitInTurnIsExempt checks that a receive turn waiting on
// its own state machine's driver is not refused by the enforcing await-in-turn
// policy, both through StateMachine.Receive and through AskEvent followed by
// an Await, which is how behaviors that host a machine drive it.
func TestDriverFutureAwaitInTurnIsExempt(t *testing.T) {
	prev := actor.AwaitInTurnWarn
	actor.SetAwaitInTurnPolicy(actor.AwaitInTurnError)
	t.Cleanup(func() {
		actor.SetAwaitInTurnPolicy(prev)
	})

	machine := NewStateMachine(StateMachineCfg[struct{}, struct{},
		*queryTestEnv]{
		Logger:        btclog.Disabled,
		ErrorReporter: queryTestReporter{},
		InitialState:  &slowState{},
		Env:           &queryTestEnv{},
	})
	machine.Start(t.Context())
	t.Cleanup(machine.Stop)

	turnCtx, end := actor.WithTurnForTest(t.Context(), "host")
	defer end()

	t.Run("Receive", func(t *testing.T) {
		_, err := machine.Receive(
			turnCtx, ActorMessage[struct{}]{},
		).Unpack()
		require.NoError(t, err)
	})

	t.Run("AskEvent Await", func(t *testing.T) {
		_, err := machine.AskEvent(turnCtx, struct{}{}).Await(
			turnCtx,
		).Unpack()
		require.NoError(t, err)
	})

	t.Run("canceled context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(turnCtx)
		cancel()

		_, err := machine.AskEvent(ctx, struct{}{}).Await(ctx).Unpack()
		require.Error(t, err)
	})
}

// awaitState is a state whose transition awaits an incomplete future with the
// driver's context, as a real machine does when it calls another actor.
type awaitState struct {
	// result receives the outcome of the await and whether the context
	// still carried a turn.
	result chan awaitOutcome
}

// awaitOutcome is what awaitState observed.
type awaitOutcome struct {
	err    error
	inTurn bool
}

// ProcessEvent awaits a future completed shortly after.
func (s *awaitState) ProcessEvent(ctx context.Context, _ struct{},
	_ *queryTestEnv) (*StateTransition[
	struct{},
	struct{},
	*queryTestEnv,
], error) {

	p := actor.NewPromise[int]()
	go func() {
		time.Sleep(20 * time.Millisecond)
		p.Complete(fn.Ok(1))
	}()

	_, inTurn := actor.TurnActor(ctx)
	_, err := p.Future().Await(ctx).Unpack()
	s.result <- awaitOutcome{err: err, inTurn: inTurn}

	return &StateTransition[struct{}, struct{}, *queryTestEnv]{
		NextState: s,
	}, nil
}

// IsTerminal implements State.
func (s *awaitState) IsTerminal() bool { return false }

// String implements State.
func (s *awaitState) String() string { return "AwaitState" }

// TestStartStripsHostTurn checks that a machine started with a receive-turn
// context does not run its driver as the host actor: the driver's own awaits
// are not refused by the enforcing policy and carry no turn, so they cannot
// register a wait edge for the host.
func TestStartStripsHostTurn(t *testing.T) {
	prev := actor.AwaitInTurnWarn
	actor.SetAwaitInTurnPolicy(actor.AwaitInTurnError)
	t.Cleanup(func() {
		actor.SetAwaitInTurnPolicy(prev)
	})

	state := &awaitState{result: make(chan awaitOutcome, 1)}
	machine := NewStateMachine(StateMachineCfg[struct{}, struct{},
		*queryTestEnv]{
		Logger:        btclog.Disabled,
		ErrorReporter: queryTestReporter{},
		InitialState:  state,
		Env:           &queryTestEnv{},
	})

	turnCtx, end := actor.WithTurnForTest(t.Context(), "host")
	defer end()

	machine.Start(turnCtx)
	t.Cleanup(machine.Stop)

	machine.SendEvent(t.Context(), struct{}{})

	select {
	case got := <-state.result:
		require.NoError(t, got.err)
		require.False(t, got.inTurn)

	case <-time.After(5 * time.Second):
		t.Fatal("driver never ran the transition")
	}
}
