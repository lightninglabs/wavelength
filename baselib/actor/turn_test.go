package actor

import (
	"context"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// TestTurnMarker asserts that the runtime marks the context it hands to a
// behavior as that actor's turn, that the marker reports the innermost actor
// when one actor asks another, and that the marker goes inactive once the turn
// returns even though the context value is still reachable.
func TestTurnMarker(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	_, ok := TurnActor(ctx)
	require.False(t, ok, "background context must not be a turn")

	// The inner actor reports the turn it sees, and hands its context out
	// so the test can check it after the turn is over.
	innerCtxs := make(chan context.Context, 1)
	inner := NewActor(ActorConfig[*testMsg, string]{
		ID: "inner",
		Behavior: NewFunctionBehavior(
			func(ctx context.Context,
				_ *testMsg) fn.Result[string] {

				innerCtxs <- ctx
				id, _ := TurnActor(ctx)

				return fn.Ok(id)
			},
		),
		MailboxSize: 1,
	})
	inner.Start()
	t.Cleanup(inner.Stop)

	// The outer actor asks the inner one from inside its own turn, so the
	// inner turn context is derived from a context that already carries
	// the outer turn.
	outer := NewActor(ActorConfig[*testMsg, string]{
		ID: "outer",
		Behavior: NewFunctionBehavior(
			func(ctx context.Context,
				_ *testMsg) fn.Result[string] {

				outerID, ok := TurnActor(ctx)
				require.True(t, ok)
				require.Equal(t, "outer", outerID)

				return inner.Ref().Ask(
					ctx, newTestMsg("ping"),
				).Await(ctx)
			},
		),
		MailboxSize: 1,
	})
	outer.Start()
	t.Cleanup(outer.Stop)

	askCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	got, err := outer.Ref().Ask(askCtx, newTestMsg("go")).Await(
		askCtx,
	).Unpack()
	require.NoError(t, err)
	require.Equal(t, "inner", got, "inner turn must shadow outer turn")

	// The inner turn has returned by the time the outer turn completed, so
	// its context no longer counts as a turn.
	innerCtx := <-innerCtxs
	_, ok = TurnActor(innerCtx)
	require.False(t, ok, "ended turn must not report as active")
}

// TestWithTurnForTest asserts the exported test helper marks and unmarks a
// context the same way the runtime does.
func TestWithTurnForTest(t *testing.T) {
	t.Parallel()

	ctx, end := WithTurnForTest(context.Background(), "probe")

	id, ok := TurnActor(ctx)
	require.True(t, ok)
	require.Equal(t, "probe", id)

	end()

	_, ok = TurnActor(ctx)
	require.False(t, ok)
}

// ctxKeyForTest is a context key for the WithoutTurn test.
type ctxKeyForTest struct{}

// TestWithoutTurn checks that WithoutTurn clears the turn marker and an
// await exemption, keeps other values and cancellation, and returns a context
// with no turn unchanged.
func TestWithoutTurn(t *testing.T) {
	plain := context.WithValue(
		context.Background(), ctxKeyForTest{}, "kept",
	)
	require.Equal(t, plain, WithoutTurn(plain))

	turnCtx, end := WithTurnForTest(plain, "host")
	defer end()

	turnCtx = AllowAwaitInTurn(turnCtx, "test")
	turnCtx, cancel := context.WithCancel(turnCtx)

	stripped := WithoutTurn(turnCtx)

	_, ok := TurnActor(stripped)
	require.False(t, ok)
	require.False(t, awaitAllowed(stripped))
	require.Equal(t, "kept", stripped.Value(ctxKeyForTest{}))

	// The original is untouched.
	_, ok = TurnActor(turnCtx)
	require.True(t, ok)

	// Cancellation still propagates.
	cancel()
	require.Error(t, stripped.Err())
}
