package protofsm

import (
	"context"
	"fmt"

	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightningnetwork/lnd/fn/v2"
)

// ActorMessage wraps an Event, in order to create a new message that can be
// used with the actor package.
type ActorMessage[Event any] struct {
	actor.BaseMessage

	// Event is the event that is being sent to the actor.
	Event Event

	// StateQuery indicates querying for the current state of the FSM.
	StateQuery bool
}

// ActorResponse is the response type for FSM actor messages.
type ActorResponse[InternalEvent any, OutboxEvent any, Env any] struct {
	// CurrentState is the current state of the FSM.
	CurrentState State[InternalEvent, OutboxEvent, Env]
}

// MessageType returns the type of the message.
//
// NOTE: This implements the actor.Message interface.
func (a ActorMessage[Event]) MessageType() string {
	return fmt.Sprintf("ActorMessage(%T)", a.Event)
}

// ActorOutboxEvent defines the interface that outbox events for the actor-based
// state machine must implement, the dispatch method can be used to deliver the
// event to the actor system or router.
type ActorOutboxEvent interface {
	Dispatch(ctx context.Context, system *actor.ActorSystem) error
}

// DeliveryMode indicates how the routed event should be delivered.
type DeliveryMode int

const (
	// DeliveryModeTell indicates a fire-and-forget delivery mode.
	DeliveryModeTell DeliveryMode = iota
	// DeliveryModeAsk indicates an ask delivery mode, which waits for a
	// response.
	DeliveryModeAsk
)

// RoutedOutboxEvent is a helper that delivers an outbox event to actors
// registered under a specific service key.
type RoutedOutboxEvent[M actor.Message, R any] struct {
	key  actor.ServiceKey[M, R]
	msg  M
	mode DeliveryMode
}

// NewTellOutboxEvent creates a fire-and-forget routed event.
func NewTellOutboxEvent[M actor.Message, R any](key actor.ServiceKey[M, R],
	msg M) RoutedOutboxEvent[M, R] {

	return RoutedOutboxEvent[M, R]{
		key:  key,
		msg:  msg,
		mode: DeliveryModeTell,
	}
}

// NewAskOutboxEvent creates an ask routed event.
func NewAskOutboxEvent[M actor.Message, R any](key actor.ServiceKey[M, R],
	msg M) RoutedOutboxEvent[M, R] {

	return RoutedOutboxEvent[M, R]{
		key:  key,
		msg:  msg,
		mode: DeliveryModeAsk,
	}
}

// Dispatch sends the event to the actor(s) registered under the service key.
func (e RoutedOutboxEvent[M, R]) Dispatch(ctx context.Context,
	system *actor.ActorSystem) error {

	// Create a router for the service key.
	router := actor.NewRouter(
		system.Receptionist(), e.key,
		actor.NewRoundRobinStrategy[M, R](), system.DeadLetters(),
	)

	switch e.mode {
	case DeliveryModeTell:
		router.Tell(ctx, e.msg)

		return nil

	case DeliveryModeAsk:
		res := router.Ask(ctx, e.msg).Await(ctx)
		if _, err := res.Unpack(); err != nil {
			return err
		}

		return nil

	default:
		return fmt.Errorf("unknown delivery mode %v", e.mode)
	}
}

// ActorStateMachine is a wrapper around a state machine that implements the
// actor.Actor interface, the state machine is only driven by incoming messages.
type ActorStateMachine[InternalEvent any, OutboxEvent ActorOutboxEvent, Env any] struct {
	sm           *StateMachine[InternalEvent, OutboxEvent, Env]
	system       *actor.ActorSystem
	currentState State[InternalEvent, OutboxEvent, Env]
}

// TellRefEnv is an environment that can hold a tell-only actor reference. This
// let's the ActorStateMachine constructor set the reference after creation.
type TellRefEnv[InternalEvent any] interface {
	SetTellOnlyRef(actor.TellOnlyRef[ActorMessage[InternalEvent]])

	GetTellOnlyRef() actor.TellOnlyRef[ActorMessage[InternalEvent]]
}

// FullRefEnv is an environment that can hold a full actor reference. This let's
// the ActorStateMachine constructor set the reference after creation.
type FullRefEnv[InternalEvent any, OutboxEvent ActorOutboxEvent, Env any] interface {
	SetActorRef(
		actor.ActorRef[
			ActorMessage[InternalEvent],
			ActorResponse[InternalEvent, OutboxEvent, Env],
		])

	GetActorRef() actor.ActorRef[
		ActorMessage[InternalEvent],
		ActorResponse[InternalEvent, OutboxEvent, Env],
	]
}

// SystemActorsStateMachine registers a new state machine actor and returns its
// reference.
func NewSystemsActorStateMachine[
	InternalEvent any,
	OutboxEvent ActorOutboxEvent,
	Env Environment,
](
	ctx context.Context,
	cfg StateMachineCfg[InternalEvent, OutboxEvent, Env],
	system *actor.ActorSystem,
	id string) actor.ActorRef[
	ActorMessage[InternalEvent],
	ActorResponse[InternalEvent, OutboxEvent, Env],
] {

	machine := NewStateMachine(cfg)
	sm := &ActorStateMachine[InternalEvent, OutboxEvent, Env]{
		sm:           &machine,
		system:       system,
		currentState: cfg.InitialState,
	}

	ref := actor.RegisterWithSystem(
		system, id, actor.NewServiceKey[
			ActorMessage[InternalEvent],
			ActorResponse[InternalEvent, OutboxEvent, Env],
		](
			id,
		),
		sm,
	)
	extraInfo := ""
	if envAny, ok := any(cfg.Env).(TellRefEnv[InternalEvent]); ok {
		envAny.SetTellOnlyRef(ref)
		extraInfo = "(tell ref env)"
	}
	if envAny, ok := any(cfg.Env).(FullRefEnv[InternalEvent, OutboxEvent, Env]); ok {
		envAny.SetActorRef(ref)
		extraInfo = "(full ref env)"
	}

	cfg.Logger.DebugS(ctx, "Setting up FSM %s", extraInfo)

	return ref
}

// Receive processes an incoming actor message and drives the state machine.
// This method implements the actor.Actor interface. any new outbox events
// generated by the state machine are dispatched to their targets within
// the actors actor system.
func (sm *ActorStateMachine[InternalEvent, OutboxEvent, Env]) Receive(
	ctx context.Context,
	e ActorMessage[InternalEvent]) fn.Result[ActorResponse[
	InternalEvent,
	OutboxEvent,
	Env,
]] {

	// If this is a state query, return the current state.
	if e.StateQuery {
		return fn.Ok(ActorResponse[InternalEvent, OutboxEvent, Env]{
			CurrentState: sm.currentState,
		})
	}

	newState, outBoxEvents, err := sm.sm.applyEvents(
		ctx, sm.currentState, e.Event,
	)
	if err != nil {
		return fn.NewResult(
			ActorResponse[InternalEvent, OutboxEvent, Env]{}, err,
		)
	}

	sm.currentState = newState

	for _, out := range outBoxEvents {
		if err := out.Dispatch(ctx, sm.system); err != nil {
			sm.sm.cfg.ErrorReporter.ReportError(err)

			return fn.NewResult(
				ActorResponse[
					InternalEvent,
					OutboxEvent,
					Env,
				]{},
				err,
			)
		}
	}

	return fn.Ok(ActorResponse[InternalEvent, OutboxEvent, Env]{
		CurrentState: sm.currentState,
	})
}
