package actor

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// TestAllowAwaitInTurn checks that the opt-out lets one incomplete wait
// through under the enforcing policy, that an empty reason does not, and that
// the exemption lasts only for the context it was derived on.
func TestAllowAwaitInTurn(t *testing.T) {
	setAwaitPolicyForTest(t, AwaitInTurnError)

	turnCtx, end := WithTurnForTest(context.Background(), "allow-actor")
	defer end()

	t.Run("exempt wait completes", func(t *testing.T) {
		p, fut := incompleteFuture()
		go func() {
			time.Sleep(5 * time.Millisecond)
			p.Complete(fn.Ok(7))
		}()

		ctx := AllowAwaitInTurn(turnCtx, "the producer is a goroutine")
		v, err := fut.Await(ctx).Unpack()
		require.NoError(t, err)
		require.Equal(t, 7, v)
	})

	t.Run("empty reason is ignored", func(t *testing.T) {
		_, fut := incompleteFuture()

		ctx := AllowAwaitInTurn(turnCtx, "")
		_, err := fut.Await(ctx).Unpack()
		require.ErrorIs(t, err, ErrAwaitInTurn)
	})

	t.Run("parent context stays guarded", func(t *testing.T) {
		_ = AllowAwaitInTurn(turnCtx, "scoped to the derived context")

		_, fut := incompleteFuture()
		_, err := fut.Await(turnCtx).Unpack()
		require.ErrorIs(t, err, ErrAwaitInTurn)
	})
}

// TestAllowAwaitInTurnNotInheritedByCallee checks that the exemption riding
// along on an Ask does not exempt the callee's own waits.
func TestAllowAwaitInTurnNotInheritedByCallee(t *testing.T) {
	setAwaitPolicyForTest(t, AwaitInTurnError)

	var (
		mu  sync.Mutex
		got error
	)

	callee := NewActor(ActorConfig[*cycleMsg, string]{
		ID: "callee",
		Behavior: NewFunctionBehavior(func(ctx context.Context,
			_ *cycleMsg) fn.Result[string] {

			_, fut := incompleteFuture()
			_, err := fut.Await(ctx).Unpack()

			mu.Lock()
			got = err
			mu.Unlock()

			return fn.Ok("done")
		}),
		MailboxSize: 1,
	})
	callee.Start()
	t.Cleanup(callee.Stop)

	turnCtx, end := WithTurnForTest(context.Background(), "caller")
	defer end()

	ctx := AllowAwaitInTurn(turnCtx, "the callee replies without waiting")
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	res := callee.Ref().Ask(ctx, &cycleMsg{kind: "go"}).Await(ctx)
	require.NoError(t, res.Err())

	mu.Lock()
	defer mu.Unlock()
	require.ErrorIs(t, got, ErrAwaitInTurn)
}

// TestAllowAwaitInTurnStillDetectsCycles checks that an exempt wait is still
// tracked: two actors that each await the other under the exemption fail with
// a wait cycle instead of hanging, and no edge is left behind.
func TestAllowAwaitInTurnStillDetectsCycles(t *testing.T) {
	setAwaitPolicyForTest(t, AwaitInTurnError)

	refs := make(map[string]ActorRef[*ringMsg, string])
	for id, next := range map[string]string{"x": "y", "y": "x"} {
		a := NewActor(ActorConfig[*ringMsg, string]{
			ID: id,
			Behavior: NewFunctionBehavior(func(ctx context.Context,
				msg *ringMsg) fn.Result[string] {

				if msg.kind == "tail" {
					return fn.Ok("tail")
				}

				kind := "go"
				if id == "y" {
					kind = "tail"
				}

				fut := refs[next].Ask(ctx, &ringMsg{kind: kind})

				waitCtx, cancel := context.WithTimeout(
					ctx, waitLong,
				)
				defer cancel()

				waitCtx = AllowAwaitInTurn(
					waitCtx, "test of the exempt wait",
				)

				return fut.Await(waitCtx)
			}),
			MailboxSize: 4,
		})
		a.Start()
		t.Cleanup(a.Stop)

		refs[id] = a.Ref()
	}

	ctx, cancel := context.WithTimeout(context.Background(), waitLong)
	defer cancel()

	begin := time.Now()
	res := refs["x"].Ask(ctx, &ringMsg{kind: "go"}).Await(ctx)
	require.ErrorIs(t, res.Err(), ErrWaitCycle)
	require.Less(t, time.Since(begin), waitFast)

	requireNoEdges(t)
}
