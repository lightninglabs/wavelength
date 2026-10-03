// Package protofsm is a minimal stand-in for the state machine package.
package protofsm

import "context"

type State[E any, O any, Env any] interface {
	ProcessEvent(ctx context.Context, event E, env Env) (State[E, O, Env],
		error)
	IsTerminal() bool
	String() string
}

type ActorOutboxEvent interface {
	Dispatch(ctx context.Context, system any) error
}
