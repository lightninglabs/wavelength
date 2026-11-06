package actor

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// leaseJoinGate holds an extension across cancellation to model a store call
// that has not yet returned. No actor may report successful join through it.
type leaseJoinGate struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

// extend exposes the in-flight store operation and waits for explicit release.
func (g *leaseJoinGate) extend(ctx context.Context) (int64, error) {
	g.once.Do(func() { close(g.entered) })
	<-g.release

	return 0, ctx.Err()
}

// blockedLeaseStore intercepts the nontransactional actor's lease extension.
type blockedLeaseStore struct {
	*mockDeliveryStore
	gate *leaseJoinGate
}

// ExtendLease keeps one store operation alive until the test releases it.
func (s *blockedLeaseStore) ExtendLease(ctx context.Context, _, _ string,
	_ time.Duration) (int64, error) {

	return s.gate.extend(ctx)
}

// blockedTxLeaseStore retains the Read/Commit store interface while blocking
// lease extension independently of the behavior's short transaction.
type blockedTxLeaseStore struct {
	*mockTxAwareStore
	gate *leaseJoinGate
}

// ExtendLease keeps one Read/Commit lease operation alive across cancellation.
func (s *blockedTxLeaseStore) ExtendLease(ctx context.Context, _, _ string,
	_ time.Duration) (int64, error) {

	return s.gate.extend(ctx)
}

// TestDurableStopJoinsHeartbeat proves both leased execution paths retain
// worker ownership until an in-flight extension returns. A timeout never
// falsely grants the caller permission to close storage.
func TestDurableStopJoinsHeartbeat(t *testing.T) {
	for _, execPath := range []bool{false, true} {
		name := "nontransactional"
		if execPath {
			name = "read-commit"
		}
		t.Run(name, func(t *testing.T) {
			testDurableStopJoinsHeartbeat(t, execPath)
		})
	}
}

// testDurableStopJoinsHeartbeat runs one actual worker with a blocked store.
func testDurableStopJoinsHeartbeat(t *testing.T, execPath bool) {
	t.Helper()
	gate := &leaseJoinGate{
		entered: make(chan struct{}), release: make(chan struct{}),
	}
	var release sync.Once
	unblock := func() {
		release.Do(func() { close(gate.release) })
	}
	t.Cleanup(unblock)
	returned := make(chan error, 1)
	behavior := newMockBehavior(fn.Ok(1))
	behavior.onReceive = func(context.Context, *actorTestMsg) {
		<-gate.entered
		returned <- nil
	}
	var store DeliveryStore = &blockedLeaseStore{
		mockDeliveryStore: newMockDeliveryStore(), gate: gate,
	}
	if execPath {
		store = &blockedTxLeaseStore{
			mockTxAwareStore: newMockTxAwareStore(), gate: gate,
		}
	}
	cfg := DefaultDurableActorConfig(
		"heartbeat-join", behavior, store, newActorTestCodec(),
	)
	cfg.HeartbeatInterval = time.Millisecond
	if execPath {
		bound := NewTxBehavior(&execTestBehavior{
			onReceive: func(ctx context.Context, _ *actorTestMsg,
				ax Exec[DeliveryStore]) fn.Result[int] {

				<-gate.entered
				err := ax.Commit(
					ctx,
					func(context.Context,
						DeliveryStore) error {

						return nil
					},
				)
				returned <- err

				return fn.NewResult(1, err)
			},
		}, identityStoreFactory)
		cfg.Behavior = fn.NewRight[ActorBehavior[*actorTestMsg, int],
			BoundTxBehavior[*actorTestMsg, int]](
			bound,
		)
		// A pool uses leased delivery rather than the single-worker
		// Read/Commit optimization. Only one message is submitted.
		cfg.NumWorkers = 2
	}
	actor := NewDurableActor(cfg).UnwrapOrFail(t)
	actor.Start()
	t.Cleanup(func() {
		unblock()
		ctx, cancel := context.WithTimeout(
			context.Background(), time.Second,
		)
		defer cancel()
		require.NoError(t, actor.StopAndWait(ctx))
	})
	require.NoError(t, actor.Ref().Tell(t.Context(), &actorTestMsg{}))
	select {
	case err := <-returned:
		require.NoError(t, err)

	case <-time.After(time.Second):
		t.Fatal("behavior did not observe the in-flight heartbeat")
	}
	shortCtx, cancel := context.WithTimeout(
		t.Context(), 50*time.Millisecond,
	)
	defer cancel()
	require.ErrorIs(
		t, actor.StopAndWait(shortCtx), context.DeadlineExceeded,
	)
	unblock()
	joinCtx, joinCancel := context.WithTimeout(t.Context(), time.Second)
	defer joinCancel()
	require.NoError(t, actor.StopAndWait(joinCtx))
}
