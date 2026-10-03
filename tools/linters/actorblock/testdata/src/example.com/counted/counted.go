package counted

import (
	"context"

	"example.com/actor"
)

type Msg struct{ actor.BaseMessage }

type A struct{ ref actor.ActorRef[Msg, int] }

// Receive reaches a baselined helper that has grown a second Await.
func (a *A) Receive(ctx context.Context, m Msg) actor.Result[int] {
	a.two(ctx, m) // want `A\.Receive -> counted\.A\.two -> Future\.Await`
	a.one(ctx, m)

	return actor.Result[int]{}
}

func (a *A) two(ctx context.Context, m Msg) {
	a.ref.Ask(ctx, m).Await(ctx)
	a.ref.Ask(ctx, m).Await(ctx)
}

func (a *A) one(ctx context.Context, m Msg) {
	a.ref.Ask(ctx, m).Await(ctx)
}
