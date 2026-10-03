package crossuse

import (
	"context"

	"example.com/actor"
	"example.com/crosshelper"
)

type Msg struct{ actor.BaseMessage }

type A struct{ ref actor.ActorRef[Msg, int] }

// Receive reaches an Await in another package.
func (a *A) Receive(ctx context.Context, m Msg) actor.Result[int] {
	_ = crosshelper.Pure()

	return actor.Result[int]{
		Val: crosshelper.Wait(ctx, a.ref.Ask(ctx, m)), // want `A\.Receive -> crosshelper\.Wait -> Future\.Await at crosshelper\.go:\d+`
	}
}
