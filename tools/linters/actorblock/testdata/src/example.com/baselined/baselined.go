package baselined

import (
	"context"

	"example.com/actor"
)

type Msg struct{ actor.BaseMessage }

type A struct{ ref actor.ActorRef[Msg, int] }

// Receive reaches a legacy site that the baseline covers, and a new one.
func (a *A) Receive(ctx context.Context, m Msg) actor.Result[int] {
	a.legacy(ctx, m)
	a.fresh(ctx, m) // want `A\.Receive -> baselined\.A\.fresh -> Future\.Await`

	return actor.Result[int]{}
}

func (a *A) legacy(ctx context.Context, m Msg) {
	a.ref.Ask(ctx, m).Await(ctx)
}

func (a *A) fresh(ctx context.Context, m Msg) {
	a.ref.Ask(ctx, m).Await(ctx)
}
