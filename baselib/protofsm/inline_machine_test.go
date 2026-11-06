package protofsm

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// inlineTestEnv checks the transaction visible to each nested transition.
type inlineTestEnv struct {
	t    *testing.T
	tx   *sql.Tx
	fail bool
}

// inlineTestState records the successfully applied step without mutation.
type inlineTestState struct{ step int }

// String identifies the test state.
func (*inlineTestState) String() string { return "InlineTest" }

// IsTerminal allows the complete internal-event chain to run.
func (*inlineTestState) IsTerminal() bool { return false }

// ProcessEvent verifies all nested work sees the original turn transaction.
func (s *inlineTestState) ProcessEvent(ctx context.Context, step int,
	env *inlineTestEnv) (*StateTransition[int, int, *inlineTestEnv],
	error) {

	tx, ok := actor.TxFromContext(ctx)
	require.True(env.t, ok)
	require.Same(env.t, env.tx, tx)
	if env.fail && step == 2 {
		return nil, errors.New("nested transition failed")
	}
	events := EmittedEvent[int, int]{Outbox: []int{step}}
	if step < 2 {
		events.InternalEvent = []int{step + 1}
	}

	return &StateTransition[int, int, *inlineTestEnv]{
		NextState: &inlineTestState{
			step: step,
		},
		NewEvents: fn.Some(events),
	}, nil
}

// TestInlineTransitionContext checks transaction propagation across a complete
// event chain and ensures failure cannot return effects from a partial chain.
func TestInlineTransitionContext(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "commit", true: "rollback"}[fail],
			func(t *testing.T) {
				tx := &sql.Tx{}
				initial := &inlineTestState{}
				env := &inlineTestEnv{t: t, tx: tx, fail: fail}
				machine := NewInlineStateMachine(
					StateMachineCfg[
						int,
						int,
						*inlineTestEnv,
					]{
						InitialState: initial, Env: env,
						Logger: btclog.Disabled,
					},
				)
				machine.Start(t.Context())
				t.Cleanup(machine.Stop)
				ctx := actor.WithTx(t.Context(), tx)
				outbox, err := machine.AskEvent(ctx, 1).
					Await(ctx).Unpack()
				state, stateErr := machine.CurrentState()
				require.NoError(t, stateErr)
				if fail {
					require.ErrorContains(
						t, err, "nested transition",
					)
					require.Empty(t, outbox)
					require.Same(t, initial, state)

					return
				}
				require.NoError(t, err)
				require.Equal(t, []int{1, 2}, outbox)
				require.Equal(
					t, &inlineTestState{
						step: 2,
					},
					state,
				)
			},
		)
	}
}

// TestInlineTransitionDoesNotOutliveCaller verifies AskEvent cannot return
// while a transition still uses a transaction owned by its caller.
func TestInlineTransitionDoesNotOutliveCaller(t *testing.T) {
	state := &queryTestState{
		entered: make(chan struct{}), release: make(chan struct{}),
	}
	machine := NewInlineStateMachine(StateMachineCfg[
		struct{}, struct{}, *queryTestEnv,
	]{InitialState: state, Env: &queryTestEnv{}, Logger: btclog.Disabled})
	machine.Start(t.Context())
	t.Cleanup(machine.Stop)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		future := machine.AskEvent(ctx, struct{}{})
		_, err := future.Await(ctx).Unpack()
		done <- err
	}()
	<-state.entered
	select {
	case err := <-done:
		t.Fatalf("AskEvent returned before transition completion: %v",
			err)

	default:
	}
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
}

// TestInlineStopIsPermanent verifies a stopped runner cannot be resurrected
// by a later Start, shared references stop safely, and ready gate channels
// cannot bypass shutdown checks.
func TestInlineStopIsPermanent(t *testing.T) {
	machine := NewInlineStateMachine(StateMachineCfg[
		int, int, *inlineTestEnv,
	]{
		InitialState: &inlineTestState{},
		Env:          &inlineTestEnv{t: t}, Logger: btclog.Disabled,
	})
	machine.Start(t.Context())
	shared := machine
	machine.Stop()
	shared.Stop()
	machine.Start(t.Context())
	require.False(t, machine.IsRunning())
	for range 20 {
		_, err := machine.AskEvent(t.Context(), 1).
			Await(t.Context()).Unpack()
		require.ErrorIs(t, err, ErrStateMachineShutdown)
		_, err = machine.CurrentState()
		require.ErrorIs(t, err, ErrStateMachineShutdown)
	}
}

// TestInlineContendersRespectCancellation verifies an active transition owns
// the gate until completion. Cancelled event and query contenders must return
// without entering the transition or reading its tentative state.
func TestInlineContendersRespectCancellation(t *testing.T) {
	state := &queryTestState{
		entered: make(chan struct{}), release: make(chan struct{}),
	}
	machine := NewInlineStateMachine(StateMachineCfg[
		struct{}, struct{}, *queryTestEnv,
	]{InitialState: state, Env: &queryTestEnv{}, Logger: btclog.Disabled})
	machine.Start(t.Context())
	t.Cleanup(machine.Stop)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := machine.AskEvent(ctx, struct{}{}).
			Await(ctx).Unpack()
		done <- err
	}()
	select {
	case <-state.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("transition never entered")
	}

	contenderCtx, cancelContenders := context.WithTimeout(
		t.Context(), 50*time.Millisecond,
	)
	defer cancelContenders()
	queried := make(chan error, 1)
	go func() {
		_, err := machine.CurrentStateWithContext(contenderCtx)
		queried <- err
	}()
	_, err := machine.AskEvent(contenderCtx, struct{}{}).
		Await(t.Context()).Unpack()
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.ErrorIs(t, <-queried, context.DeadlineExceeded)
	_, err = machine.CurrentState()
	require.ErrorIs(t, err, context.DeadlineExceeded)
	select {
	case err := <-done:
		t.Fatalf("contender released the active transition: %v", err)

	default:
	}

	close(state.release)
	require.NoError(t, <-done)
	observed, err := machine.CurrentState()
	require.NoError(t, err)
	require.Same(t, state, observed)
}
