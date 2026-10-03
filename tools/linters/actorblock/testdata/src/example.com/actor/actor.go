// Package actor is a minimal stand-in for the real actor framework, with the
// same names and shapes the analyzer keys on.
package actor

import (
	"context"
	"time"
)

type Message interface {
	messageMarker()
}

type BaseMessage struct{}

func (BaseMessage) messageMarker() {}

type Result[T any] struct{ Val T }

type Future[T any] interface {
	Await(ctx context.Context) Result[T]
	OnComplete(ctx context.Context, fn func(Result[T]))
	ThenApply(ctx context.Context, fn func(T) T) Future[T]
}

type TellOnlyRef[M Message] interface {
	Tell(ctx context.Context, msg M) error
	TryTell(ctx context.Context, msg M) error
}

type ActorRef[M Message, R any] interface {
	TellOnlyRef[M]
	Ask(ctx context.Context, msg M) Future[R]
}

type ActorFunc[M Message, R any] func(context.Context, M) Result[R]

type FunctionBehavior[M Message, R any] struct{}

func NewFunctionBehavior[M Message, R any](
	fn ActorFunc[M, R]) *FunctionBehavior[M, R] {

	return &FunctionBehavior[M, R]{}
}

func AskThen[M Message, R any, S Message](ctx context.Context,
	ref ActorRef[M, R], msg M, self TellOnlyRef[S], timeout time.Duration,
	wrap func(Result[R]) S) {
}

// Exec is the transactional handle of a TxBehavior.
type Exec[S any] struct{}
