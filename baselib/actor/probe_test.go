package actor

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// TestProbeIdleAndStopped proves probes do not invoke business logic and
// require a running receive loop, even when no application messages ever
// arrive.
func TestProbeIdleAndStopped(t *testing.T) {
	var wg sync.WaitGroup
	a := NewActor(ActorConfig[*testMsg, string]{
		ID: "idle", Wg: &wg,
		Behavior: NewFunctionBehavior(
			func(context.Context, *testMsg) fn.Result[string] {
				t.Error("probe reached actor behavior")

				return fn.Ok("")
			},
		),
	})
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, probeError(ctx, a.Ref()), context.DeadlineExceeded)
	a.Start()
	t.Cleanup(func() { a.Stop(); wg.Wait() })
	ctx, cancel = context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	require.NoError(t, probeError(ctx, a.Ref()))
	require.NoError(t, probeError(ctx, a.Ref()))
	a.Stop()
	require.ErrorIs(t, probeError(ctx, a.Ref()), ErrActorTerminated)
}

// TestProbeStallCoalescingAndRecovery pins bounded work after timeouts. A
// blocked behavior cannot acknowledge probes; repeated and concurrent checks
// share one queued envelope. After recovery a new check must traverse the loop
// again.
func TestProbeStallCoalescingAndRecovery(t *testing.T) {
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	var wg sync.WaitGroup
	a := NewActor(
		ActorConfig[*testMsg, string]{
			ID:          "blocked",
			Wg:          &wg,
			MailboxSize: 3,
			Behavior: NewFunctionBehavior(
				func(ctx context.Context,
					_ *testMsg) fn.Result[string] {

					entered <- struct{}{}
					select {
					case <-release:
					case <-ctx.Done():
					}

					return fn.Ok("")
				},
			),
		},
	)
	a.Start()
	t.Cleanup(func() { a.Stop(); wg.Wait() })
	require.NoError(t, a.Ref().Tell(t.Context(), newTestMsg("block")))
	<-entered

	for range 3 {
		var callers sync.WaitGroup
		for range 8 {
			callers.Add(1)
			go func() {
				defer callers.Done()
				ctx, cancel := context.WithTimeout(
					t.Context(), 20*time.Millisecond,
				)
				defer cancel()
				require.ErrorIs(
					t,
					probeError(
						ctx, a.Ref(),
					),
					context.DeadlineExceeded,
				)
			}()
		}
		callers.Wait()
		mailbox, ok := a.mailbox.(*ChannelMailbox[*testMsg, string])
		require.True(t, ok)
		require.Len(t, mailbox.ch, 1)
	}

	release <- struct{}{}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	require.NoError(t, probeError(ctx, a.Ref()))

	// A previous successful acknowledgement must not mask a new wedge.
	require.NoError(t, a.Ref().Tell(t.Context(), newTestMsg("block again")))
	<-entered
	ctx, cancel = context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, probeError(ctx, a.Ref()), context.DeadlineExceeded)

	// Stopping the actor also bounds a wait whose caller has no deadline.
	result := make(chan error, 1)
	go func() {
		result <- probeError(t.Context(), a.Ref())
	}()
	a.Stop()
	select {
	case err := <-result:
		require.ErrorIs(t, err, ErrActorTerminated)

	case <-time.After(time.Second):
		t.Fatal("probe did not stop with actor")
	}
}

// TestProbeFullMailboxNeverWaits ensures health admission cannot join a mailbox
// deadlock, even when the caller has no deadline and there is no consumer.
func TestProbeFullMailboxNeverWaits(t *testing.T) {
	a := NewActor(ActorConfig[*testMsg, string]{
		ID: "full", MailboxSize: 1,
	})
	t.Cleanup(a.Stop)
	require.NoError(t, a.Ref().Tell(t.Context(), newTestMsg("fill")))
	result := make(chan error, 1)
	go func() {
		result <- probeError(t.Context(), a.Ref())
	}()
	select {
	case err := <-result:
		require.ErrorIs(t, err, ErrMailboxFull)

	case <-time.After(time.Second):
		t.Fatal("probe blocked on a full mailbox")
	}
	require.Nil(t, a.probeDone)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, probeError(ctx, a.Ref()), context.Canceled)
}

// probeError selects the response result when a test is about waiting
// semantics.
func probeError(ctx context.Context, ref ActorRef[*testMsg, string]) error {
	_, err := Probe(ctx, ref)

	return err
}

// TestProbeBusyAndLateProgress proves completed turns remain observable when
// admission fails or an acknowledgement arrives after its caller timed out.
func TestProbeBusyAndLateProgress(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var wg sync.WaitGroup
	behavior := NewFunctionBehavior(
		func(ctx context.Context, _ *testMsg) fn.Result[string] {
			entered <- struct{}{}
			select {
			case <-release:
			case <-ctx.Done():
			}

			return fn.Ok("")
		},
	)
	a := NewActor(ActorConfig[*testMsg, string]{
		ID: "busy", MailboxSize: 2, Wg: &wg, Behavior: behavior,
	})
	a.Start()
	t.Cleanup(func() { a.Stop(); wg.Wait() })
	require.NoError(t, a.Ref().Tell(t.Context(), newTestMsg("first")))
	<-entered
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	completed, err := Probe(ctx, a.Ref())
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Zero(t, completed)

	// The late acknowledgement is between two business turns. Its caller
	// has gone, but its progress must survive the next probe's timeout.
	require.NoError(t, a.Ref().Tell(t.Context(), newTestMsg("second")))
	release <- struct{}{}
	<-entered
	ctx, cancel = context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	completed, err = Probe(ctx, a.Ref())
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.EqualValues(t, 2, completed)

	// Drain the pending probe and keep the receive queue full while turns
	// finish. Every admission fails, yet the counter proves useful
	// progress.
	require.NoError(t, a.Ref().Tell(t.Context(), newTestMsg("third")))
	release <- struct{}{}
	<-entered
	for range 2 {
		require.NoError(
			t,
			a.Ref().Tell(t.Context(), newTestMsg("queued")),
		)
	}
	for i := range 3 {
		completed, err = Probe(t.Context(), a.Ref())
		require.ErrorIs(t, err, ErrMailboxFull)
		require.EqualValues(t, 4+i, completed)
		release <- struct{}{}
		<-entered
		require.NoError(
			t,
			a.Ref().Tell(t.Context(), newTestMsg("refill")),
		)
	}
}

// TestProbeRegisteredReference verifies the registration path used by the
// daemon returns a supported direct ref and preserves shutdown semantics.
func TestProbeRegisteredReference(t *testing.T) {
	system := NewActorSystem()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(
			context.Background(), time.Second,
		)
		defer cancel()
		require.NoError(t, system.Shutdown(ctx))
	})
	key := NewServiceKey[*testMsg, string]("probe-test")
	ref := RegisterWithSystem(
		system, "probe-test", key, newEchoBehavior(t, 0),
	)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	completed, err := Probe(ctx, ref)
	require.NoError(t, err)
	require.Positive(t, completed)
	require.NoError(t, system.Shutdown(ctx))
	_, err = Probe(ctx, ref)
	require.ErrorIs(t, err, ErrActorTerminated)
}
