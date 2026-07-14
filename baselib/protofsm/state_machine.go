package protofsm

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnutils"
)

const (
	// pollInterval is the interval at which we'll poll the SendWhen
	// predicate if specified.
	pollInterval = time.Millisecond * 100
)

var (
	// ErrStateMachineShutdown occurs when trying to feed an event to a
	// StateMachine that has been asked to Stop.
	ErrStateMachineShutdown = fmt.Errorf("StateMachine is shutting down")
)

// EmittedEvent is a special type that can be emitted by a state transition.
// This can container internal events which are to be routed back to the state,
// or external events which are to be sent to the daemon.
type EmittedEvent[InboxEvent any, OutboxEvent any] struct {
	// InternalEvent is an optional internal event that is to be routed
	// back to the target state. This enables state to trigger one or many
	// state transitions without a new external event.
	InternalEvent []InboxEvent

	// Outbox is an optional set of events that are accumulated during event
	// processing and returned to the caller for processing into the main
	// state machine. This enables nested state machines to emit events that
	// bubble up to their parent.
	Outbox []OutboxEvent
}

// StateTransition is a state transition type. It denotes the next state to go
// to, and also the set of events to emit.
type StateTransition[InternalEvent any, OutboxEvent any, Env Environment] struct {
	// NextState is the next state to transition to.
	NextState State[InternalEvent, OutboxEvent, Env]

	// NewEvents is the set of events to emit.
	NewEvents fn.Option[EmittedEvent[InternalEvent, OutboxEvent]]
}

// Environment is an abstract interface that represents the environment that
// the state machine will execute using. From the PoV of the main state machine
// executor, we just care about being able to clean up any resources that were
// allocated by the environment.
type Environment interface {
}

// State defines an abstract state along, namely its state transition function
// that takes as input an event and an environment, and returns a state
// transition (next state, and set of events to emit). As state can also either
// be terminal, or not, a terminal event causes state execution to halt.
type State[InternalEvent any, OutboxEvent any, Env Environment] interface {
	// ProcessEvent takes an event and an environment, and returns a new
	// state transition. This will be iteratively called until either a
	// terminal state is reached, or no further internal events are
	// emitted.
	ProcessEvent(ctx context.Context, event InternalEvent, env Env) (
		*StateTransition[InternalEvent, OutboxEvent, Env], error)

	// IsTerminal returns true if this state is terminal, and false
	// otherwise.
	IsTerminal() bool

	// String returns a human readable string that represents the state.
	String() string
}

// stateQuery is used by outside callers to query the internal state of the
// state machine.
type stateQuery[InternalEvent any, OutboxEvent any, Env Environment] struct {
	// CurrentState is a channel that will be sent the current state of the
	// state machine.
	CurrentState chan State[InternalEvent, OutboxEvent, Env]
}

// syncEventRequest is used to send an event to the state machine synchronously,
// waiting for the event processing to complete and returning the accumulated
// outbox events.
type syncEventRequest[InternalEvent any, OutboxEvent any] struct {
	// event is the event to process.
	event InternalEvent

	// promise is used to signal completion and return the accumulated
	// outbox events or an error.
	promise actor.Promise[[]OutboxEvent]
}

// StateMachine represents an abstract FSM that is able to process new incoming
// events and drive a state machine to termination. This implementation uses
// type params to abstract over the types of events and environment. Events
// trigger new state transitions, that use the environment to perform some
// action.
//
// TODO(roasbeef): terminal check, daemon event execution, init?
type StateMachine[InternalEvent any, OutboxEvent any,
	Env Environment] struct {
	cfg StateMachineCfg[InternalEvent, OutboxEvent, Env]

	log btclog.Logger

	// events is the channel that will be used to send new events to the
	// FSM.
	events chan InternalEvent

	// syncEvents is the channel that will be used to send synchronous event
	// requests to the FSM, returning the accumulated outbox events.
	syncEvents chan syncEventRequest[InternalEvent, OutboxEvent]

	// newStateEvents is an EventDistributor that will be used to notify
	// any relevant callers of new state transitions that occur.
	newStateEvents *fn.EventDistributor[State[
		InternalEvent, OutboxEvent, Env]]

	// stateQuery is a channel that will be used by outside callers to
	// query the internal state machine state.
	stateQuery chan stateQuery[InternalEvent, OutboxEvent, Env]

	gm   fn.GoroutineManager
	quit chan struct{}

	// startOnce and stopOnce are used to ensure that the state machine is
	// only started and stopped once.
	startOnce sync.Once
	stopOnce  sync.Once

	// running is a flag that indicates if the state machine is currently
	// running.
	running atomic.Bool
}

// ErrorReporter is an interface that's used to report errors that occur during
// state machine execution.
type ErrorReporter interface {
	// ReportError is a method that's used to report an error that occurred
	// during state machine execution.
	ReportError(err error)
}

// StateMachineCfg is a configuration struct that's used to create a new state
// machine.
type StateMachineCfg[InternalEvent any, OutboxEvent any, Env Environment] struct {
	// Logger is used for logging.
	Logger btclog.Logger

	// ErrorReporter is used to report errors that occur during state
	// transitions.
	ErrorReporter ErrorReporter

	// InitialState is the initial state of the state machine.
	InitialState State[InternalEvent, OutboxEvent, Env]

	// Env is the environment that the state machine will use to execute.
	Env Env

	// CustomPollInterval is an optional custom poll interval that can be
	// used to set a quicker interval for tests.
	CustomPollInterval fn.Option[time.Duration]
}

// NewStateMachine creates a new state machine given a set of daemon adapters,
// an initial state, an environment, and an event to process as if emitted at
// the onset of the state machine. Such an event can be used to set up tracking
// state such as a txid confirmation event.
func NewStateMachine[InternalEvent any, OutboxEvent any, Env Environment](
	cfg StateMachineCfg[InternalEvent, OutboxEvent, Env]) StateMachine[
	InternalEvent, OutboxEvent, Env] {

	return StateMachine[InternalEvent, OutboxEvent, Env]{
		cfg:    cfg,
		log:    cfg.Logger,
		events: make(chan InternalEvent, 1),
		syncEvents: make(
			chan syncEventRequest[InternalEvent, OutboxEvent], 1,
		),
		stateQuery: make(
			chan stateQuery[InternalEvent, OutboxEvent, Env],
		),
		gm: *fn.NewGoroutineManager(),
		newStateEvents: fn.NewEventDistributor[State[
			InternalEvent, OutboxEvent, Env]](),
		quit: make(chan struct{}),
	}
}

// Start starts the state machine. This will spawn a goroutine that will drive
// the state machine to completion.
func (s *StateMachine[InternalEvent, OutboxEvent, Env]) Start(
	ctx context.Context) {

	s.startOnce.Do(func() {
		_ = s.gm.Go(ctx, func(ctx context.Context) {
			s.driveMachine(ctx)
		})

		s.running.Store(true)
	})
}

// Stop stops the state machine. This will block until the state machine has
// reached a stopping point.
func (s *StateMachine[InternalEvent, OutboxEvent, Env]) Stop() {
	s.stopOnce.Do(func() {
		close(s.quit)
		s.gm.Stop()

		s.running.Store(false)
	})
}

// SendEvent sends a new event to the state machine.
//
// TODO(roasbeef): bool if processed?
func (s *StateMachine[InternalEvent, OutboxEvent, Env]) SendEvent(
	ctx context.Context, event InternalEvent) {

	s.log.Debugf("Sending event %T", event)

	select {
	case s.events <- event:
	case <-ctx.Done():
		return

	case <-s.quit:
		return
	}
}

// AskEvent sends a new event to the state machine using the Ask pattern
// (request-response), waiting for the event to be fully processed. It returns a
// Future that will be resolved with the accumulated outbox events from all
// state transitions triggered by this event, including nested internal events.
// The Future's Await method will return fn.Result[[]OutboxEvent] containing
// either the accumulated outbox events or an error if processing failed.
func (s *StateMachine[InternalEvent, OutboxEvent, Env]) AskEvent(
	ctx context.Context, event InternalEvent) actor.Future[[]OutboxEvent] {

	s.log.Debugf("Asking event %T", event)

	// Create a promise to signal completion and return results.
	promise := actor.NewPromise[[]OutboxEvent]()

	req := syncEventRequest[InternalEvent, OutboxEvent]{
		event:   event,
		promise: promise,
	}

	// Check for context cancellation or shutdown first to avoid races.
	select {
	case <-ctx.Done():
		promise.Complete(
			fn.Errf[[]OutboxEvent](
				"context cancelled: %w", ctx.Err(),
			),
		)

		return promise.Future()

	case <-s.quit:
		promise.Complete(fn.Err[[]OutboxEvent](ErrStateMachineShutdown))

		return promise.Future()

	default:
	}

	// Send the request to the state machine. If we can't send it due to
	// context cancellation or shutdown, complete the promise with an error.
	select {
	// Successfully sent, the promise will be completed by driveMachine.
	case s.syncEvents <- req:
	case <-ctx.Done():
		promise.Complete(
			fn.Errf[[]OutboxEvent](
				"context cancelled: %w", ctx.Err(),
			),
		)

	case <-s.quit:
		promise.Complete(fn.Err[[]OutboxEvent](ErrStateMachineShutdown))
	}

	return promise.Future()
}

// Receive processes a message and returns a Result containing the accumulated
// outbox events from the state machine. The provided context is the actor's
// internal context, which can be used to detect actor shutdown requests.
//
// This method uses the AskEvent pattern to wait for the event to be fully
// processed and collect any outbox events emitted during state transitions.
// This enables the actor system to propagate events from nested state machines
// up through the actor hierarchy.
//
// NOTE: This implements the actor.ActorBehavior interface.
func (s *StateMachine[InternalEvent, OutboxEvent, Env]) Receive(
	ctx context.Context,
	e ActorMessage[InternalEvent]) fn.Result[[]OutboxEvent] {

	// Use AskEvent to process the event and get the outbox events back.
	future := s.AskEvent(ctx, e.Event)

	// Await the result which will contain the accumulated outbox events
	// from all state transitions triggered by this event.
	return future.Await(ctx)
}

// CurrentState returns the current state of the state machine.
func (s *StateMachine[InternalEvent, OutboxEvent, Env]) CurrentState() (
	State[InternalEvent, OutboxEvent, Env], error) {

	query := stateQuery[InternalEvent, OutboxEvent, Env]{
		CurrentState: make(
			chan State[InternalEvent, OutboxEvent, Env], 1,
		),
	}

	if !fn.SendOrQuit(s.stateQuery, query, s.quit) {
		return nil, ErrStateMachineShutdown
	}

	return fn.RecvOrTimeout(query.CurrentState, time.Second)
}

// StateSubscriber represents an active subscription to be notified of new
// state transitions.
type StateSubscriber[InternalEvent any, OutboxEvent any, Env Environment] *fn.EventReceiver[State[InternalEvent, OutboxEvent, Env]]

// RegisterStateEvents registers a new event listener that will be notified of
// new state transitions.
func (s *StateMachine[
	InternalEvent,
	OutboxEvent,
	Env,
]) RegisterStateEvents() StateSubscriber[InternalEvent, OutboxEvent, Env] {

	subscriber := fn.NewEventReceiver[State[
		InternalEvent,
		OutboxEvent,
		Env,
	]](
		10,
	)

	// TODO(roasbeef): instead give the state and the input event?

	s.newStateEvents.RegisterSubscriber(subscriber)

	return subscriber
}

// RemoveStateSub removes the target state subscriber from the set of active
// subscribers.
func (s *StateMachine[InternalEvent, OutboxEvent, Env]) RemoveStateSub(
	sub StateSubscriber[InternalEvent, OutboxEvent, Env]) {

	_ = s.newStateEvents.RemoveSubscriber(sub)
}

// IsRunning returns true if the state machine is currently running.
func (s *StateMachine[InternalEvent, OutboxEvent, Env]) IsRunning() bool {
	return s.running.Load()
}

// applyEvents applies a new event to the state machine. This will continue
// until no further events are emitted by the state machine. Along the way,
// we'll also ensure to execute any daemon events that are emitted. The
// function returns the final state, any accumulated outbox events, and an
// error if one occurred.
func (s *StateMachine[InternalEvent, OutboxEvent, Env]) applyEvents(
	ctx context.Context,
	currentState State[InternalEvent, OutboxEvent, Env],
	newEvent InternalEvent) (State[InternalEvent, OutboxEvent, Env],
	[]OutboxEvent, error) {

	eventQueue := fn.NewQueue(newEvent)

	// outbox accumulates all outbox events from state transitions during
	// the entire event processing chain.
	var outbox []OutboxEvent

	// Given the next event to handle, we'll process the event, then add
	// any new emitted internal events to our event queue. This continues
	// until we reach a terminal state, or we run out of internal events to
	// process.
	//
	//nolint:ll
	for nextEvent := eventQueue.Dequeue(); nextEvent.IsSome(); nextEvent = eventQueue.Dequeue() {
		err := fn.MapOptionZ(nextEvent, func(event InternalEvent) error {
			// Log the event type at debug level to keep
			// steady-state output compact. The full event payload
			// (which may include auth blobs, signatures, etc.) is
			// only spewed at trace.
			s.log.DebugS(ctx, "Processing event",
				btclog.Fmt("event_type", "%T", event),
			)
			s.log.TraceS(
				ctx, "Processing event payload", "event",
				lnutils.SpewLogClosure(event),
			)

			// Apply the state transition function of the current
			// state given this new event and our existing env.
			transition, err := currentState.ProcessEvent(
				ctx, event, s.cfg.Env,
			)
			if err != nil {
				return err
			}

			newEvents := transition.NewEvents
			err = fn.MapOptionZ(newEvents, func(
				events EmittedEvent[
					InternalEvent,
					OutboxEvent,
				],
			) error {

				// Next, we'll add any new emitted events to our
				// event queue. As above, debug only carries the
				// event type; the full body is left to trace.
				for _, inEvent := range events.InternalEvent {
					s.log.DebugS(
						ctx,
						"Adding new internal event to "+
							"queue",
						btclog.Fmt(
							"event_type", "%T",
							inEvent,
						),
					)
					s.log.TraceS(
						ctx, "Internal event payload",
						"event",
						lnutils.SpewLogClosure(inEvent),
					)

					eventQueue.Enqueue(inEvent)
				}

				// Accumulate any outbox events from this state
				// transition.
				outbox = append(outbox, events.Outbox...)

				return nil
			})
			if err != nil {
				return err
			}

			s.log.InfoS(ctx, "State transition",
				btclog.Fmt("from_state", "%v", currentState),
				btclog.Fmt(
					"to_state", "%v", transition.NextState,
				),
			)

			// With our events processed, we'll now update our
			// internal state.
			currentState = transition.NextState

			// Notify our subscribers of the new state transition.
			//
			// TODO(roasbeef): will only give us the outer state?
			//  * let FSMs choose which state to emit?
			s.newStateEvents.NotifySubscribers(currentState)

			return nil
		})
		if err != nil {
			return currentState, nil, err
		}
	}

	return currentState, outbox, nil
}

// driveMachine is the main event loop of the state machine. It accepts any new
// incoming events, and then drives the state machine forward until it reaches
// a terminal state.
func (s *StateMachine[InternalEvent, OutboxEvent, Env]) driveMachine(
	ctx context.Context) {

	s.log.DebugS(ctx, "Starting state machine")

	currentState := s.cfg.InitialState

	// We just started driving the state machine, so we'll notify our
	// subscribers of this starting state.
	s.newStateEvents.NotifySubscribers(currentState)

	for {
		select {
		// We have a new external event, so we'll drive the state
		// machine forward until we either run out of internal events,
		// or we reach a terminal state.
		case newEvent := <-s.events:
			newState, _, err := s.applyEvents(
				ctx, currentState, newEvent,
			)
			if err != nil {
				s.cfg.ErrorReporter.ReportError(err)

				s.log.ErrorS(ctx, "Unable to apply event", err)

				// An error occurred, so we'll tear down the
				// entire state machine as we can't proceed.
				go s.Stop()

				return
			}

			currentState = newState

		// We have a synchronous event request that expects the
		// accumulated outbox events to be returned via the promise.
		case syncReq := <-s.syncEvents:
			newState, outbox, err := s.applyEvents(
				ctx, currentState, syncReq.event,
			)
			if err != nil {
				s.cfg.ErrorReporter.ReportError(err)

				s.log.ErrorS(ctx, "Unable to apply sync event",
					err,
				)

				// Complete the promise with the error.
				//
				// TODO(roasbeef): distinguish between error
				// types? state vs processing
				syncReq.promise.Complete(
					fn.Err[[]OutboxEvent](err),
				)

				// An error occurred, so we'll tear down the
				// entire state machine as we can't proceed.
				go s.Stop()

				return
			}

			currentState = newState

			// Complete the promise with the accumulated outbox
			// events.
			syncReq.promise.Complete(fn.Ok(outbox))

		// An outside caller is querying our state, so we'll return the
		// latest state.
		case stateQuery := <-s.stateQuery:
			if !fn.SendOrQuit(
				stateQuery.CurrentState, currentState, s.quit,
			) {
				return
			}

		case <-s.gm.Done():
			return
		}
	}
}
