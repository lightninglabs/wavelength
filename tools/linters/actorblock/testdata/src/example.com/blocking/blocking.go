package blocking

import (
	"context"
	"sync"
	"time"

	"example.com/actor"
)

type Msg struct{ actor.BaseMessage }

type ref = actor.ActorRef[Msg, int]

type Direct struct {
	ref ref
	ctx context.Context
	wg  sync.WaitGroup
}

// Receive awaits directly.
func (d *Direct) Receive(ctx context.Context, m Msg) actor.Result[int] {
	return d.ref.Ask(ctx, m).Await(ctx) // want `Direct\.Receive can block its turn: Direct\.Receive -> Future\.Await at blocking\.go:\d+`
}

type ViaHelper struct{ ref ref }

// Receive awaits through two helpers.
func (v *ViaHelper) Receive(ctx context.Context, m Msg) actor.Result[int] {
	return v.outer(ctx, m) // want `ViaHelper\.Receive -> blocking\.ViaHelper\.outer -> blocking\.ViaHelper\.inner -> Future\.Await at blocking\.go:\d+`
}

func (v *ViaHelper) outer(ctx context.Context, m Msg) actor.Result[int] {
	return v.inner(ctx, m)
}

func (v *ViaHelper) inner(ctx context.Context, m Msg) actor.Result[int] {
	return v.ref.Ask(ctx, m).Await(ctx)
}

type Async struct {
	ref ref
	wg  sync.WaitGroup
}

// Receive only awaits off the actor goroutine.
func (a *Async) Receive(ctx context.Context, m Msg) actor.Result[int] {
	go func() {
		a.ref.Ask(ctx, m).Await(ctx)
	}()

	fut := a.ref.Ask(ctx, m)
	fut.OnComplete(ctx, func(r actor.Result[int]) {
		fut.Await(ctx)
	})
	fut.ThenApply(ctx, func(i int) int {
		fut.Await(ctx)

		return i
	})
	actor.AskThen(ctx, a.ref, m, nil, time.Second,
		func(r actor.Result[int]) Msg {
			fut.Await(ctx)

			return Msg{}
		},
	)
	a.wg.Go(func() {
		fut.Await(ctx)
	})

	return actor.Result[int]{}
}

type Unreached struct{ ref ref }

// Receive does not call helper.
func (u *Unreached) Receive(ctx context.Context, m Msg) actor.Result[int] {
	return actor.Result[int]{}
}

// helper awaits but nothing on a turn calls it.
func (u *Unreached) helper(ctx context.Context, m Msg) {
	u.ref.Ask(ctx, m).Await(ctx)
}

type Allow struct{ ref ref }

// Receive uses the allow directive with and without a reason.
func (a *Allow) Receive(ctx context.Context, m Msg) actor.Result[int] {
	//actor:allow-await the callee is a leaf actor that never calls back
	a.ref.Ask(ctx, m).Await(ctx)

	a.ref.Ask(ctx, m).Await(ctx) //actor:allow-await leaf actor

	a.ref.Ask(ctx, m).Await(ctx) /* want `requires a non-empty reason` `Allow\.Receive -> Future\.Await` */ //actor:allow-await

	//actor:allow-send not an await directive
	a.ref.Ask(ctx, m).Await(ctx) // want `Allow\.Receive -> Future\.Await`

	return a.guarded(ctx, m)
}

func (a *Allow) guarded(ctx context.Context, m Msg) actor.Result[int] {
	//actor:allow-await exempting the site silences every caller
	return a.ref.Ask(ctx, m).Await(ctx)
}

type Sends struct {
	ref ref
	ctx context.Context
}

// Receive sends with a variety of contexts.
func (s *Sends) Receive(ctx context.Context, m Msg) actor.Result[int] {
	_ = s.ref.Tell(context.Background(), m) // want `Sends\.Receive -> Tell with a non-turn context at blocking\.go:\d+`
	_ = s.ref.Tell(context.TODO(), m)       // want `Tell with a non-turn context`
	_ = s.ref.Tell(s.ctx, m)                // want `Tell with a non-turn context`

	bg := context.Background()
	_ = s.ref.Tell(bg, m) // want `Tell with a non-turn context`

	wrapped, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = s.ref.Tell(wrapped, m) // want `Tell with a non-turn context`

	// None of these are reported.
	_ = s.ref.Tell(ctx, m)
	_ = s.ref.TryTell(context.Background(), m)

	timed, cancel2 := context.WithTimeout(ctx, time.Second)
	defer cancel2()
	_ = s.ref.Tell(timed, m)
	_ = s.ref.Tell(context.WithoutCancel(ctx), m)

	//actor:allow-send detached on purpose, the target never replies
	_ = s.ref.Tell(context.Background(), m)

	return actor.Result[int]{}
}

type SendHelper struct{ ref ref }

// Receive reaches a send with a bad context through a helper.
func (s *SendHelper) Receive(ctx context.Context, m Msg) actor.Result[int] {
	s.notify(m) // want `SendHelper\.Receive -> blocking\.SendHelper\.notify -> Tell with a non-turn context`

	return actor.Result[int]{}
}

func (s *SendHelper) notify(m Msg) {
	_ = s.ref.Tell(context.Background(), m)
}

// NewBehavior builds a function behavior.
func NewBehavior(r ref) *actor.FunctionBehavior[Msg, int] {
	return actor.NewFunctionBehavior(
		func(ctx context.Context, m Msg) actor.Result[int] {
			return r.Ask(ctx, m).Await(ctx) // want `function behavior passed to NewFunctionBehavior can block its turn`
		},
	)
}

// NotAnActor has a Receive that is not an actor behavior.
type NotAnActor struct{ ref ref }

func (n *NotAnActor) Receive(ctx context.Context, m string) int {
	return n.ref.Ask(ctx, Msg{}).Await(ctx).Val
}

// Tx is a transactional behavior.
type Tx struct{ ref ref }

// Receive awaits, and has the extra Exec parameter.
func (t *Tx) Receive(ctx context.Context, m Msg,
	ax actor.Exec[int]) actor.Result[int] {

	return t.ref.Ask(ctx, m).Await(ctx) // want `Tx\.Receive -> Future\.Await`
}

// NewNamedBehavior builds a function behavior from a named function.
func NewNamedBehavior() *actor.FunctionBehavior[Msg, int] {
	return actor.NewFunctionBehavior(handle)
}

func handle(ctx context.Context, m Msg) actor.Result[int] {
	var r ref

	return r.Ask(ctx, m).Await(ctx) // want `function behavior passed to NewFunctionBehavior can block its turn`
}

type shared struct{ ref ref }

func (s *shared) wait(ctx context.Context, m Msg) {
	s.ref.Ask(ctx, m).Await(ctx)
}

type CallerAllow struct{ s shared }

// Receive exempts only its own path to the shared helper.
func (c *CallerAllow) Receive(ctx context.Context, m Msg) actor.Result[int] {
	//actor:allow-await this caller only talks to a leaf actor
	c.s.wait(ctx, m)

	return actor.Result[int]{}
}

type CallerDeny struct{ s shared }

// Receive reaches the same helper without a directive.
func (c *CallerDeny) Receive(ctx context.Context, m Msg) actor.Result[int] {
	c.s.wait(ctx, m) // want `CallerDeny\.Receive -> blocking\.shared\.wait -> Future\.Await`

	return actor.Result[int]{}
}
