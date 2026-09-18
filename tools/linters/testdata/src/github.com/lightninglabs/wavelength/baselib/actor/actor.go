package actor

import (
	"context"
	"io"

	"github.com/lightningnetwork/lnd/tlv"
)

type TLVMessage interface {
	TLVType() tlv.Type
	Encode(io.Writer) error
	Decode(io.Reader) error
}

type MessageCodec struct{}

func NewMessageCodec() *MessageCodec {
	return &MessageCodec{}
}

func (*MessageCodec) Register(tlv.Type, func() TLVMessage) error {
	return nil
}

func (*MessageCodec) MustRegister(tlv.Type, func() TLVMessage) {}

type Future[T any] struct{}

func (Future[T]) Await(context.Context) error {
	return nil
}

func (Future[T]) OnComplete(context.Context, func(error)) {}

func (Future[T]) ThenApply(context.Context, func(T) T) Future[T] {
	return Future[T]{}
}

type ActorRef[M, R any] struct{}

func (ActorRef[M, R]) Ask(context.Context, M) Future[R] {
	return Future[R]{}
}

func (ActorRef[M, R]) Tell(context.Context, M) error {
	return nil
}

type Exec[S any] interface {
	Read(context.Context, func(context.Context, S) error) error
	Stage(context.Context, func(context.Context, S) error) error
	Commit(context.Context, func(context.Context, S) error) error
}

type ActorBehavior[M, R any] interface {
	Receive(context.Context, M) R
}

type DurableActorConfig[M, R any] struct{}

func DefaultDurableActorConfig[M, R any](string, ActorBehavior[M, R],
	any, *MessageCodec) DurableActorConfig[M, R] {

	return DurableActorConfig[M, R]{}
}

func NewClassicBehavior[M, R any](
	behavior ActorBehavior[M, R]) ActorBehavior[M, R] {

	return behavior
}

func (DurableActorConfig[M, R]) AllowConcurrentClassicBehavior() DurableActorConfig[M, R] {

	return DurableActorConfig[M, R]{}
}

type DetachedAsk[R any] struct {
	CallerCtx context.Context
}

func DetachAskPromise[R any](context.Context) (DetachedAsk[R], bool) {
	return DetachedAsk[R]{}, true
}

func WithoutTx(ctx context.Context) context.Context {
	return ctx
}
