package protofsm

import (
	"context"
	"sync/atomic"

	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightningnetwork/lnd/fn/v2"
)

// InlineStateMachine drives protocol transitions on the owning actor's turn.
// It never starts a worker goroutine or dispatches an outbox. In particular,
// the transition receives the caller's transaction context for its entire
// internal-event chain, and processing has finished when AskEvent returns.
// The actor remains responsible for checkpointing the returned state together
// with its effects and for restoring the checkpoint after a failed commit.
type InlineStateMachine[E any, O any, Env Environment] struct {
	driver StateMachine[E, O, Env]
	state  State[E, O, Env]
	gate   chan struct{}
	quit   chan struct{}
	// lifecycle is new (0), running (1), or permanently stopped (2).
	lifecycle atomic.Uint32
}

// NewInlineStateMachine constructs a synchronous transition runner. The
// original threaded runner remains available for independently driven FSMs.
func NewInlineStateMachine[E any, O any, Env Environment](
	cfg StateMachineCfg[E, O, Env]) InlineStateMachine[E, O, Env] {

	gate := make(chan struct{}, 1)
	gate <- struct{}{}

	return InlineStateMachine[E, O, Env]{
		driver: NewStateMachine(cfg), state: cfg.InitialState,
		gate: gate, quit: make(chan struct{}),
	}
}

// Start enables processing without capturing a process or request context.
func (s *InlineStateMachine[E, O, Env]) Start(ctx context.Context) {
	if ctx.Err() == nil {
		s.lifecycle.CompareAndSwap(0, 1)
	}
}

// Stop prevents further transitions. There is no worker goroutine to join;
// the owning actor's turn must finish before its runner is discarded.
func (s *InlineStateMachine[E, O, Env]) Stop() {
	if s.lifecycle.Swap(2) != 2 {
		close(s.quit)
	}
}

// IsRunning reports whether the runner accepts actor turns.
func (s *InlineStateMachine[E, O, Env]) IsRunning() bool {
	return s.lifecycle.Load() == 1
}

// AskEvent applies the complete internal-event chain synchronously and returns
// its completed result. Unlike the threaded runner, this method cannot return
// while a transition still holds the caller's transaction context.
func (s *InlineStateMachine[E, O, Env]) AskEvent(ctx context.Context,
	event E) actor.Future[[]O] {

	promise := actor.NewPromise[[]O]()
	result, err := s.apply(ctx, event)
	promise.Complete(fn.NewResult(result, err))

	return promise.Future()
}

// apply serializes transitions and only publishes a new in-memory state after
// the entire internal-event chain succeeds. States must not mutate their input
// state in place if they need rollback without a checkpoint reload.
func (s *InlineStateMachine[E, O, Env]) apply(ctx context.Context, event E) (
	[]O, error) {

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !s.IsRunning() {
		return nil, ErrStateMachineShutdown
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()

	case <-s.quit:
		return nil, ErrStateMachineShutdown

	case <-s.gate:
	}
	defer func() {
		s.gate <- struct{}{}
	}()
	if !s.IsRunning() {
		return nil, ErrStateMachineShutdown
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	next, outbox, err := s.driver.applyEvents(ctx, s.state, event)
	if err != nil {
		return nil, err
	}
	s.state = next

	return outbox, nil
}

// CurrentState returns a serialized view, bounded like the threaded runner.
func (s *InlineStateMachine[E, O, Env]) CurrentState() (State[E, O, Env],
	error) {

	ctx, cancel := context.WithTimeout(
		context.Background(), DefaultStateQueryTimeout,
	)
	defer cancel()

	return s.CurrentStateWithContext(ctx)
}

// CurrentStateWithContext returns the current state within the caller's bound.
func (s *InlineStateMachine[E, O, Env]) CurrentStateWithContext(
	ctx context.Context) (State[E, O, Env], error) {

	select {
	case <-ctx.Done():
		return nil, ctx.Err()

	case <-s.quit:
		return nil, ErrStateMachineShutdown

	case <-s.gate:
		defer func() {
			s.gate <- struct{}{}
		}()

		if s.lifecycle.Load() == 2 {
			return nil, ErrStateMachineShutdown
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		return s.state, nil
	}
}
