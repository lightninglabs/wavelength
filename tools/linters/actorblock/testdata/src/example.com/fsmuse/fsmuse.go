package fsmuse

import (
	"context"

	"example.com/actor"
	"example.com/protofsm"
)

type Msg struct{ actor.BaseMessage }

type env struct{ ref actor.ActorRef[Msg, int] }

type idle struct{}

// ProcessEvent awaits in a state handler.
func (s *idle) ProcessEvent(ctx context.Context, ev Msg,
	e *env) (protofsm.State[Msg, int, *env], error) {

	e.ref.Ask(ctx, ev).Await(ctx) // want `idle\.ProcessEvent can block its turn: idle\.ProcessEvent -> Future\.Await`

	return s, nil
}

func (s *idle) IsTerminal() bool { return false }
func (s *idle) String() string   { return "idle" }

// other has a ProcessEvent but is not a state, since it lacks the rest of
// the interface.
type other struct{}

func (o *other) ProcessEvent(ctx context.Context, ev Msg, e *env) {
	e.ref.Ask(ctx, ev).Await(ctx)
}

type outbox struct{ ref actor.ActorRef[Msg, int] }

// Dispatch awaits in an outbox event.
func (o outbox) Dispatch(ctx context.Context, system any) error {
	o.ref.Ask(ctx, Msg{}).Await(ctx) // want `outbox\.Dispatch can block its turn`

	return nil
}
