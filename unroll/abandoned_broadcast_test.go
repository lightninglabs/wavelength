package unroll

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/chainsource"
	"github.com/lightninglabs/wavelength/txconfirm"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// scriptedCancelRef controls one cancellation boundary in cleanup tests.
type scriptedCancelRef struct {
	calls atomic.Int32
	mu    sync.Mutex
	ids   []string
	ask   func(context.Context, int32) actor.Future[txconfirm.Resp]
}

// ID identifies the scripted reference.
func (r *scriptedCancelRef) ID() string { return "scripted-cancel" }

// Tell is unused because cleanup requires an acknowledgement.
func (r *scriptedCancelRef) Tell(context.Context, txconfirm.Msg) error {
	return nil
}

// TryTell delegates to Tell for this test double.
func (r *scriptedCancelRef) TryTell(ctx context.Context,
	msg txconfirm.Msg) error {

	return r.Tell(ctx, msg)
}

// Ask runs the per-attempt script.
func (r *scriptedCancelRef) Ask(ctx context.Context,
	msg txconfirm.Msg) actor.Future[txconfirm.Resp] {

	req, ok := msg.(*txconfirm.CancelInterestReq)
	if !ok {
		panic("scripted cancel ref received a non-cancel request")
	}
	r.mu.Lock()
	r.ids = append(r.ids, req.SubscriberID)
	r.mu.Unlock()

	return r.ask(ctx, r.calls.Add(1))
}

// subscriberIDs returns the IDs used across all cancellation attempts.
func (r *scriptedCancelRef) subscriberIDs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]string(nil), r.ids...)
}

// removalResponse shortens the test double's generic future signatures.
type removalResponse = chainsource.ChainSourceResp

// removalFuture is the test double's wallet-removal reply.
type removalFuture = actor.Future[removalResponse]

// scriptedRemovalRef controls the wallet-removal acknowledgement.
type scriptedRemovalRef struct {
	calls atomic.Int32
	ask   func(context.Context, int32) removalFuture
}

// ID identifies the scripted reference.
func (r *scriptedRemovalRef) ID() string { return "scripted-removal" }

// Tell is unused because cleanup requires an acknowledgement.
func (r *scriptedRemovalRef) Tell(context.Context,
	chainsource.ChainSourceMsg) error {

	return nil
}

// TryTell delegates to Tell for this test double.
func (r *scriptedRemovalRef) TryTell(ctx context.Context,
	msg chainsource.ChainSourceMsg) error {

	return r.Tell(ctx, msg)
}

// Ask runs the per-attempt script.
func (r *scriptedRemovalRef) Ask(ctx context.Context,
	_ chainsource.ChainSourceMsg) removalFuture {

	return r.ask(ctx, r.calls.Add(1))
}

// cancelResult returns an acknowledged cancellation with the given ownership.
func cancelResult(remaining int) actor.Future[txconfirm.Resp] {
	promise := actor.NewPromise[txconfirm.Resp]()
	promise.Complete(
		fn.Ok[txconfirm.Resp](
			&txconfirm.CancelInterestResp{
				RemainingSubscribers: remaining,
			},
		),
	)

	return promise.Future()
}

// removalResult acknowledges wallet removal.
func removalResult() actor.Future[chainsource.ChainSourceResp] {
	promise := actor.NewPromise[chainsource.ChainSourceResp]()
	promise.Complete(
		fn.Ok[chainsource.ChainSourceResp](
			&chainsource.RemoveTxResponse{},
		),
	)

	return promise.Future()
}

// TestAbandonedBroadcastCleanupRetriesBusyActor proves an admission timeout
// does not leave the terminal exit's broadcast interest alive.
func TestAbandonedBroadcastCleanupRetriesBusyActor(t *testing.T) {
	var requested atomic.Int32
	cancelRef := &scriptedCancelRef{
		ask: func(ctx context.Context,
			call int32) actor.Future[txconfirm.Resp] {

			if call == 1 {
				<-ctx.Done()
				promise := actor.NewPromise[txconfirm.Resp]()
				promise.Complete(
					fn.Err[txconfirm.Resp](
						ctx.Err(),
					),
				)

				return promise.Future()
			}
			requested.Add(1)

			return cancelResult(0)
		},
	}
	removalRef := &scriptedRemovalRef{
		ask: func(context.Context,
			int32) actor.Future[chainsource.ChainSourceResp] {

			return removalResult()
		},
	}

	ctx, stop := context.WithTimeout(t.Context(), time.Second)
	defer stop()
	cleanupAbandonedBroadcast(
		ctx, nil, cancelRef, removalRef, "exit-a", chainhash.Hash{1},
		true, 10*time.Millisecond, time.Millisecond, btclog.Disabled,
	)
	require.NoError(t, ctx.Err())
	require.EqualValues(t, 2, cancelRef.calls.Load())
	require.Equal(
		t, []string{"exit-a", "exit-a"}, cancelRef.subscriberIDs(),
	)
	require.EqualValues(t, 1, requested.Load())
	require.EqualValues(t, 1, removalRef.calls.Load())
}

// TestAbandonedBroadcastCleanupRetriesLostReply proves a late first Ask is
// safe: the retry uses the same subscriber ID and observes no other owner.
func TestAbandonedBroadcastCleanupRetriesLostReply(t *testing.T) {
	late := actor.NewPromise[txconfirm.Resp]()
	cancelRef := &scriptedCancelRef{
		ask: func(_ context.Context,
			call int32) actor.Future[txconfirm.Resp] {

			if call == 1 {
				return late.Future()
			}

			return cancelResult(0)
		},
	}
	removalRef := &scriptedRemovalRef{
		ask: func(context.Context,
			int32) actor.Future[chainsource.ChainSourceResp] {

			return removalResult()
		},
	}

	ctx, stop := context.WithTimeout(t.Context(), time.Second)
	defer stop()
	cleanupAbandonedBroadcast(
		ctx, nil, cancelRef, removalRef, "exit-a", chainhash.Hash{2},
		true, 10*time.Millisecond, time.Millisecond, btclog.Disabled,
	)
	late.Complete(
		fn.Ok[txconfirm.Resp](
			&txconfirm.CancelInterestResp{
				RemainingSubscribers: 0,
			},
		),
	)
	require.NoError(t, ctx.Err())
	require.EqualValues(t, 2, cancelRef.calls.Load())
	require.Equal(
		t, []string{"exit-a", "exit-a"}, cancelRef.subscriberIDs(),
	)
	require.EqualValues(t, 1, removalRef.calls.Load())
}

// TestAbandonedBroadcastCleanupRetainsSharedTransaction proves one exit's
// cancellation cannot remove a transaction another exit still needs.
func TestAbandonedBroadcastCleanupRetainsSharedTransaction(t *testing.T) {
	cancelRef := &scriptedCancelRef{
		ask: func(context.Context, int32) actor.Future[txconfirm.Resp] {
			return cancelResult(1)
		},
	}
	removalRef := &scriptedRemovalRef{
		ask: func(context.Context,
			int32) actor.Future[chainsource.ChainSourceResp] {

			return removalResult()
		},
	}

	cleanupAbandonedBroadcast(
		t.Context(), nil, cancelRef, removalRef, "exit-a",
		chainhash.Hash{3}, true, time.Second, time.Millisecond,
		btclog.Disabled,
	)
	require.EqualValues(t, 1, cancelRef.calls.Load())
	require.Zero(t, removalRef.calls.Load())
}

// TestAbandonedBroadcastCleanupRetriesRemoval proves a timed-out wallet
// removal cannot silently end the cleanup after cancellation succeeds.
func TestAbandonedBroadcastCleanupRetriesRemoval(t *testing.T) {
	cancelRef := &scriptedCancelRef{
		ask: func(context.Context, int32) actor.Future[txconfirm.Resp] {
			return cancelResult(0)
		},
	}
	removalRef := &scriptedRemovalRef{
		ask: func(ctx context.Context,
			call int32) actor.Future[chainsource.ChainSourceResp] {

			if call == 1 {
				<-ctx.Done()
				promise := actor.NewPromise[removalResponse]()
				promise.Complete(
					fn.Err[chainsource.ChainSourceResp](
						ctx.Err(),
					),
				)

				return promise.Future()
			}

			return removalResult()
		},
	}

	ctx, stop := context.WithTimeout(t.Context(), time.Second)
	defer stop()
	cleanupAbandonedBroadcast(
		ctx, nil, cancelRef, removalRef, "exit-a", chainhash.Hash{4},
		true, 10*time.Millisecond, time.Millisecond, btclog.Disabled,
	)
	require.NoError(t, ctx.Err())
	require.EqualValues(t, 2, cancelRef.calls.Load())
	require.EqualValues(t, 2, removalRef.calls.Load())
}

// TestAbandonedBroadcastCleanupBoundsPermanentRemovalError proves wallet
// housekeeping cannot retry forever after broadcast interest is canceled.
func TestAbandonedBroadcastCleanupBoundsPermanentRemovalError(t *testing.T) {
	cancelRef := &scriptedCancelRef{
		ask: func(context.Context, int32) actor.Future[txconfirm.Resp] {
			return cancelResult(0)
		},
	}
	removalRef := &scriptedRemovalRef{
		ask: func(context.Context, int32) removalFuture {
			promise := actor.NewPromise[removalResponse]()
			promise.Complete(
				fn.Err[removalResponse](
					errors.New("permanent removal failure"),
				),
			)

			return promise.Future()
		},
	}

	ctx, stop := context.WithTimeout(t.Context(), time.Second)
	defer stop()
	cleanupAbandonedBroadcast(
		ctx, nil, cancelRef, removalRef, "exit-a", chainhash.Hash{6},
		true, 10*time.Millisecond, time.Millisecond, btclog.Disabled,
	)
	require.NoError(t, ctx.Err())
	require.EqualValues(
		t, abandonedBroadcastRemovalAttempts, removalRef.calls.Load(),
	)
	require.EqualValues(
		t, abandonedBroadcastRemovalAttempts, cancelRef.calls.Load(),
	)
}

// TestAbandonedBroadcastCleanupStopsWithRegistry proves a terminal child's
// cleanup does not keep retrying after its owning registry shuts down.
func TestAbandonedBroadcastCleanupStopsWithRegistry(t *testing.T) {
	started := make(chan struct{})
	registryDone := make(chan struct{})
	workerDone := make(chan struct{})
	cancelRef := &scriptedCancelRef{
		ask: func(ctx context.Context,
			_ int32) actor.Future[txconfirm.Resp] {

			close(started)
			<-ctx.Done()
			promise := actor.NewPromise[txconfirm.Resp]()
			promise.Complete(fn.Err[txconfirm.Resp](ctx.Err()))

			return promise.Future()
		},
	}
	removalRef := &scriptedRemovalRef{
		ask: func(context.Context,
			int32) actor.Future[chainsource.ChainSourceResp] {

			return removalResult()
		},
	}
	go func() {
		defer close(workerDone)
		cleanupAbandonedBroadcast(
			t.Context(), registryDone, cancelRef, removalRef,
			"exit-a", chainhash.Hash{5}, true, 10*time.Millisecond,
			time.Millisecond, btclog.Disabled,
		)
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("cleanup never started cancellation")
	}
	close(registryDone)
	select {
	case <-workerDone:
	case <-time.After(time.Second):
		t.Fatal("cleanup did not stop with registry")
	}
	require.EqualValues(t, 1, cancelRef.calls.Load())
	require.Zero(t, removalRef.calls.Load())
}
