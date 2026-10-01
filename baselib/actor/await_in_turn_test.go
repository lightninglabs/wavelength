package actor

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/build"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// The tests in this file change the process-wide await-in-turn policy, so none
// of them calls t.Parallel. Go runs a package's serial tests to completion
// before it resumes any parallel one, which keeps the policy they set from
// leaking into a test that awaits inside a turn on another goroutine.

// setAwaitPolicyForTest installs policy, forgets the call sites logged so far,
// and restores the previous policy when the test ends.
func setAwaitPolicyForTest(t *testing.T, policy AwaitInTurnPolicy) {
	t.Helper()

	prev := currentAwaitInTurnPolicy()
	SetAwaitInTurnPolicy(policy)

	awaitInTurnSites.Range(func(k, _ any) bool {
		awaitInTurnSites.Delete(k)

		return true
	})
	awaitInTurnFlagged.Store(0)

	t.Cleanup(func() {
		SetAwaitInTurnPolicy(prev)
	})
}

// incompleteFuture returns a promise and its future, which has not completed.
func incompleteFuture() (Promise[int], Future[int]) {
	p := NewPromise[int]()

	return p, p.Future()
}

// TestAwaitInTurnDefaultPolicyIsWarn pins the rollout default, which must be
// the policy that changes no behavior.
func TestAwaitInTurnDefaultPolicyIsWarn(t *testing.T) {
	require.Equal(t, AwaitInTurnWarn, AwaitInTurnPolicy(0))
}

// TestAwaitInTurnWarnLogsOncePerSite checks that the warn policy lets the
// await proceed and flags each call site once, however often it runs, while a
// different call site is flagged separately.
func TestAwaitInTurnWarnLogsOncePerSite(t *testing.T) {
	setAwaitPolicyForTest(t, AwaitInTurnWarn)

	turnCtx, end := WithTurnForTest(context.Background(), "warn-actor")
	defer end()

	// awaitAt is one call site, shared by every loop iteration below. The
	// future never completes and the await gives up on a short deadline,
	// so the await always parks and never takes the already-complete fast
	// path that skips the flag.
	awaitAt := func() fn.Result[int] {
		_, fut := incompleteFuture()

		waitCtx, cancel := context.WithTimeout(
			turnCtx, time.Millisecond,
		)
		defer cancel()

		return fut.Await(waitCtx)
	}

	for i := 0; i < 3; i++ {
		_, err := awaitAt().Unpack()
		require.ErrorIs(t, err, context.DeadlineExceeded)
	}
	require.EqualValues(t, 1, awaitInTurnFlagged.Load())

	// A second line of code is a second site.
	_, fut := incompleteFuture()
	waitCtx, cancel := context.WithTimeout(turnCtx, time.Millisecond)
	defer cancel()

	_, err := fut.Await(waitCtx).Unpack()
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.EqualValues(t, 2, awaitInTurnFlagged.Load())
}

// TestAwaitInTurnWarnReportsBehaviorSite checks the logged record names the
// actor and the line of the code that called Await, not a frame inside the
// package, and points the reader at the alternatives.
func TestAwaitInTurnWarnReportsBehaviorSite(t *testing.T) {
	setAwaitPolicyForTest(t, AwaitInTurnWarn)

	var buf bytes.Buffer
	log := btclog.NewSLogger(btclog.NewDefaultHandler(&buf))
	log.SetLevel(btclog.LevelInfo)

	ctx := build.ContextWithLogger(context.Background(), log)
	turnCtx, end := WithTurnForTest(ctx, "site-actor")
	defer end()

	waitCtx, cancel := context.WithTimeout(turnCtx, time.Millisecond)
	defer cancel()

	_, fut := incompleteFuture()
	_ = fut.Await(waitCtx)

	out := buf.String()
	require.Contains(t, out, "site-actor")
	require.Contains(t, out, "AskThen")
	require.Contains(t, out, "await_in_turn_test.go:")
	require.NotContains(t, out, "future.go:")
	require.EqualValues(t, 1, awaitInTurnFlagged.Load())
}

// TestAwaitInTurnAllowIsSilent checks that the allow policy neither flags nor
// fails an await inside a turn.
func TestAwaitInTurnAllowIsSilent(t *testing.T) {
	setAwaitPolicyForTest(t, AwaitInTurnAllow)

	turnCtx, end := WithTurnForTest(context.Background(), "allow-actor")
	defer end()

	ctx, cancel := context.WithTimeout(turnCtx, 5*time.Millisecond)
	defer cancel()

	_, fut := incompleteFuture()
	_, err := fut.Await(ctx).Unpack()
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Zero(t, awaitInTurnFlagged.Load())
}

// TestAwaitInTurnErrorPolicy covers the enforcing policy: an incomplete
// future fails at once, a complete one still yields its value, and an await
// outside a turn, or with the context of a turn that already ended, is
// untouched.
func TestAwaitInTurnErrorPolicy(t *testing.T) {
	setAwaitPolicyForTest(t, AwaitInTurnError)

	turnCtx, end := WithTurnForTest(context.Background(), "error-actor")

	t.Run("incomplete future fails fast", func(t *testing.T) {
		_, fut := incompleteFuture()

		// The deadline only turns a regression, where the await
		// parks, into a failure instead of a hang.
		waitCtx, cancel := context.WithTimeout(turnCtx, time.Second)
		defer cancel()

		_, err := fut.Await(waitCtx).Unpack()
		require.ErrorIs(t, err, ErrAwaitInTurn)
		require.NotErrorIs(t, err, context.DeadlineExceeded)
	})

	t.Run("complete future returns its value", func(t *testing.T) {
		p, fut := incompleteFuture()
		p.Complete(fn.Ok(42))

		v, err := fut.Await(turnCtx).Unpack()
		require.NoError(t, err)
		require.Equal(t, 42, v)
	})

	t.Run("outside a turn is unaffected", func(t *testing.T) {
		p, fut := incompleteFuture()
		go func() {
			time.Sleep(5 * time.Millisecond)
			p.Complete(fn.Ok(5))
		}()

		v, err := fut.Await(context.Background()).Unpack()
		require.NoError(t, err)
		require.Equal(t, 5, v)
	})

	// End the turn: its context now waits like any other.
	end()

	t.Run("ended turn is unaffected", func(t *testing.T) {
		p, fut := incompleteFuture()
		go func() {
			time.Sleep(5 * time.Millisecond)
			p.Complete(fn.Ok(6))
		}()

		v, err := fut.Await(turnCtx).Unpack()
		require.NoError(t, err)
		require.Equal(t, 6, v)
	})
}

// TestAwaitInTurnErrorPolicyInRealActor checks the guard against a context the
// runtime stamped, not one built by the test helper.
func TestAwaitInTurnErrorPolicyInRealActor(t *testing.T) {
	setAwaitPolicyForTest(t, AwaitInTurnError)

	var (
		mu  sync.Mutex
		got error
	)
	done := make(chan struct{})

	beh := NewFunctionBehavior(func(ctx context.Context,
		_ *cycleMsg) fn.Result[string] {

		_, fut := incompleteFuture()
		_, err := fut.Await(ctx).Unpack()

		mu.Lock()
		got = err
		mu.Unlock()
		close(done)

		return fn.Ok("ok")
	})

	a := NewActor(ActorConfig[*cycleMsg, string]{
		ID: "guard-actor", Behavior: beh, MailboxSize: 1,
	})
	a.Start()
	t.Cleanup(a.Stop)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	require.NoError(t, a.Ref().Tell(ctx, &cycleMsg{kind: "go"}))

	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("behavior never ran")
	}

	mu.Lock()
	defer mu.Unlock()
	require.ErrorIs(t, got, ErrAwaitInTurn)
}

// TestAwaitInTurnFrameworkAwaitsAreExempt checks that ThenApply, OnComplete and
// MapRef.Ask, which await on helper goroutines using the caller's context, do
// not trip the enforcing policy when that context belongs to a running turn.
func TestAwaitInTurnFrameworkAwaitsAreExempt(t *testing.T) {
	setAwaitPolicyForTest(t, AwaitInTurnError)

	turnCtx, end := WithTurnForTest(context.Background(), "exempt-actor")
	defer end()

	t.Run("ThenApply", func(t *testing.T) {
		p, fut := incompleteFuture()
		next := fut.ThenApply(turnCtx, func(v int) int { return v + 1 })

		time.Sleep(10 * time.Millisecond)
		p.Complete(fn.Ok(1))

		waitCtx, cancel := context.WithTimeout(
			context.Background(), 5*time.Second,
		)
		defer cancel()

		v, err := next.Await(waitCtx).Unpack()
		require.NoError(t, err)
		require.Equal(t, 2, v)
	})

	t.Run("OnComplete", func(t *testing.T) {
		p, fut := incompleteFuture()
		res := make(chan fn.Result[int], 1)
		fut.OnComplete(turnCtx, func(r fn.Result[int]) {
			res <- r
		})

		time.Sleep(10 * time.Millisecond)
		p.Complete(fn.Ok(3))

		select {
		case r := <-res:
			v, err := r.Unpack()
			require.NoError(t, err)
			require.Equal(t, 3, v)

		case <-time.After(5 * time.Second):
			t.Fatal("callback never ran")
		}
	})

	t.Run("MapRef.Ask", func(t *testing.T) {
		release := make(chan struct{})
		beh := NewFunctionBehavior(func(_ context.Context,
			_ *cycleMsg) fn.Result[string] {

			<-release

			return fn.Ok("mapped")
		})
		a := NewActor(ActorConfig[*cycleMsg, string]{
			ID: "mapref-target", Behavior: beh, MailboxSize: 1,
		})
		a.Start()
		t.Cleanup(a.Stop)

		ref := TypeAssertingRef[Message, *cycleMsg, string](a.Ref())
		fut := ref.Ask(turnCtx, &cycleMsg{kind: "q"})

		// Give the helper goroutine time to park on the inner
		// future while the turn is still active.
		time.Sleep(10 * time.Millisecond)
		close(release)

		waitCtx, cancel := context.WithTimeout(
			context.Background(), 5*time.Second,
		)
		defer cancel()

		v, err := fut.Await(waitCtx).Unpack()
		require.NoError(t, err)
		require.Equal(t, "mapped", v)
	})
}
