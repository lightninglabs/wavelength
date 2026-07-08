//go:build walletdkrpc && swapruntime

package swapwallet

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/db"
	"github.com/lightninglabs/darepo-client/rpc/walletdkrpc"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

// fakeSubscribeStream is a minimal WalletService_SubscribeWalletServer that
// records every response. When release is non-nil the FIRST Send blocks on it
// (after recording), so a test can wedge the live loop mid-send and overflow
// the subscriber buffer deterministically.
type fakeSubscribeStream struct {
	grpc.ServerStream

	ctx context.Context

	mu   sync.Mutex
	sent []*walletdkrpc.SubscribeWalletResponse

	entered chan struct{}
	release chan struct{}
	blocked bool
}

// Context returns the stream context.
func (f *fakeSubscribeStream) Context() context.Context {
	return f.ctx
}

// Send records the response and, on the first call when a release gate is set,
// signals entry and blocks until released.
func (f *fakeSubscribeStream) Send(
	r *walletdkrpc.SubscribeWalletResponse) error {

	f.mu.Lock()
	f.sent = append(f.sent, r)
	first := !f.blocked && f.release != nil
	if first {
		f.blocked = true
	}
	f.mu.Unlock()

	if first {
		if f.entered != nil {
			close(f.entered)
		}
		<-f.release
	}

	return nil
}

// responses returns a copy of everything sent so far.
func (f *fakeSubscribeStream) responses() []*walletdkrpc.SubscribeWalletResponse {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append(
		[]*walletdkrpc.SubscribeWalletResponse(nil), f.sent...,
	)
}

// newSubscribeFixture builds a Service backed by a real activity store so the
// resumable event-log replay can be exercised end to end. buffer overrides the
// per-subscriber channel size (0 uses the default).
func newSubscribeFixture(t *testing.T, buffer uint32) (*Service,
	*db.ActivityPersistenceStore, *Runtime) {

	t.Helper()

	testDB := db.NewTestDB(t)
	store := db.NewStore(
		testDB.DB, testDB.Queries, testDB.Backend(), btclog.Disabled,
	).NewActivityStore(clock.NewDefaultClock())

	deps := &Deps{
		SwapService:     &fakeSwapService{},
		RPCServer:       &fakeRPCServer{},
		ActivityStore:   store,
		SubscribeBuffer: buffer,
	}
	runtime := newRuntime(t.Context(), deps)
	t.Cleanup(runtime.stop)

	return newService(deps, runtime), store, runtime
}

// projectTestEntry writes one transition into the store and returns its
// event_seq, so a test can assert the cursor a replay/live update carries.
func projectTestEntry(t *testing.T, store *db.ActivityPersistenceStore,
	id string, kind walletdkrpc.EntryKind,
	status walletdkrpc.EntryStatus) int64 {

	t.Helper()

	proj, err := entryToProjection(&walletdkrpc.WalletEntry{
		Id:            id,
		Kind:          kind,
		Status:        status,
		CreatedAtUnix: 100,
		UpdatedAtUnix: 100,
	})
	require.NoError(t, err)

	seq, err := store.ProjectEntry(context.Background(), proj)
	require.NoError(t, err)

	return seq
}

// TestReplayStart pins the cursor/include_existing → replay-start mapping.
func TestReplayStart(t *testing.T) {
	t.Parallel()

	from, replay := replayStart(&walletdkrpc.SubscribeWalletRequest{
		Cursor: 7,
	})
	require.True(t, replay, "a non-zero cursor replays")
	require.Equal(t, int64(7), from)

	from, replay = replayStart(&walletdkrpc.SubscribeWalletRequest{
		IncludeExisting: true,
	})
	require.True(t, replay, "include_existing replays the full history")
	require.Equal(t, int64(0), from)

	from, replay = replayStart(&walletdkrpc.SubscribeWalletRequest{})
	require.False(
		t, replay, "no cursor and no include_existing is live-only",
	)
	require.Equal(t, int64(0), from)
}

// TestSubscribeReplaysFromCursor verifies the event-log replay: from cursor 0
// it streams every transition oldest-first with ascending cursors, and from a
// prior cursor it streams only what came after — the resume path.
func TestSubscribeReplaysFromCursor(t *testing.T) {
	t.Parallel()

	svc, store, _ := newSubscribeFixture(t, 0)

	// Three transitions: a new deposit, a new send, then the send settling.
	s1 := projectTestEntry(
		t, store, "dep", walletdkrpc.
			EntryKind_ENTRY_KIND_DEPOSIT,
		walletdkrpc.EntryStatus_ENTRY_STATUS_COMPLETE,
	)
	s2 := projectTestEntry(
		t, store, "snd", walletdkrpc.
			EntryKind_ENTRY_KIND_SEND,
		walletdkrpc.EntryStatus_ENTRY_STATUS_PENDING,
	)
	s3 := projectTestEntry(
		t, store, "snd", walletdkrpc.
			EntryKind_ENTRY_KIND_SEND,
		walletdkrpc.EntryStatus_ENTRY_STATUS_COMPLETE,
	)

	// Full replay from 0.
	full := &fakeSubscribeStream{ctx: context.Background()}
	last, err := svc.replayEvents(context.Background(), full, 0, nil)
	require.NoError(t, err)
	require.Equal(t, s3, last, "replay returns the highest cursor sent")

	resp := full.responses()
	require.Len(t, resp, 3)
	require.Equal(t, s1, resp[0].GetCursor())
	require.Equal(t, "dep", resp[0].GetEntry().GetId())
	require.Equal(t, s3, resp[2].GetCursor())
	require.Equal(
		t, walletdkrpc.EntryStatus_ENTRY_STATUS_COMPLETE,
		resp[2].GetEntry().GetStatus(),
	)

	// Resume from s2: only the settling transition (s3) is new.
	resume := &fakeSubscribeStream{ctx: context.Background()}
	last, err = svc.replayEvents(context.Background(), resume, s2, nil)
	require.NoError(t, err)
	require.Equal(t, s3, last)

	only := resume.responses()
	require.Len(t, only, 1, "resume replays only events after the cursor")
	require.Equal(t, s3, only[0].GetCursor())
	require.Equal(t, "snd", only[0].GetEntry().GetId())
}

// TestSubscribeReplayHonorsKindFilter verifies the kind filter gates the
// replayed events.
func TestSubscribeReplayHonorsKindFilter(t *testing.T) {
	t.Parallel()

	svc, store, _ := newSubscribeFixture(t, 0)

	projectTestEntry(
		t, store, "dep", walletdkrpc.
			EntryKind_ENTRY_KIND_DEPOSIT,
		walletdkrpc.EntryStatus_ENTRY_STATUS_COMPLETE,
	)
	projectTestEntry(
		t, store, "snd", walletdkrpc.
			EntryKind_ENTRY_KIND_SEND,
		walletdkrpc.EntryStatus_ENTRY_STATUS_COMPLETE,
	)

	filter, err := buildKindFilter([]walletdkrpc.EntryKind{
		walletdkrpc.EntryKind_ENTRY_KIND_DEPOSIT,
	})
	require.NoError(t, err)

	stream := &fakeSubscribeStream{ctx: context.Background()}
	_, err = svc.replayEvents(context.Background(), stream, 0, filter)
	require.NoError(t, err)

	resp := stream.responses()
	require.Len(t, resp, 1, "only the DEPOSIT event passes the filter")
	require.Equal(
		t, walletdkrpc.EntryKind_ENTRY_KIND_DEPOSIT,
		resp[0].GetEntry().GetKind(),
	)
}

// TestSubscribeStreamsLiveUpdate verifies a live transition after subscribe is
// streamed with its event_seq as the cursor.
func TestSubscribeStreamsLiveUpdate(t *testing.T) {
	t.Parallel()

	svc, _, runtime := newSubscribeFixture(t, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stream := &fakeSubscribeStream{ctx: ctx}
	done := make(chan error, 1)
	go func() {
		done <- svc.SubscribeWallet(
			&walletdkrpc.SubscribeWalletRequest{}, stream,
		)
	}()

	// A live transition after subscribe must reach the stream.
	runtime.projectAndEmit(context.Background(), &walletdkrpc.WalletEntry{
		Id:            "live",
		Kind:          walletdkrpc.EntryKind_ENTRY_KIND_EXIT,
		Status:        walletdkrpc.EntryStatus_ENTRY_STATUS_PENDING,
		CreatedAtUnix: 100,
		UpdatedAtUnix: 100,
	})

	require.Eventually(t, func() bool {
		resp := stream.responses()

		return len(resp) == 1 && resp[0].GetEntry().GetId() == "live"
	}, time.Second, 10*time.Millisecond)

	resp := stream.responses()
	require.Positive(
		t, resp[0].GetCursor(),
		"a live update carries its event_seq as the cursor",
	)

	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
}

// TestSubscribeSignalsGapOnOverflow verifies that when a consumer falls behind
// (its send buffer overflows), it receives a SubscribeGap carrying the resume
// cursor rather than losing updates silently.
func TestSubscribeSignalsGapOnOverflow(t *testing.T) {
	t.Parallel()

	// A one-slot buffer makes overflow deterministic.
	svc, _, runtime := newSubscribeFixture(t, 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stream := &fakeSubscribeStream{
		ctx:     ctx,
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	done := make(chan error, 1)
	go func() {
		done <- svc.SubscribeWallet(
			&walletdkrpc.SubscribeWalletRequest{}, stream,
		)
	}()

	emit := func(id string) {
		runtime.projectAndEmit(context.Background(),
			&walletdkrpc.WalletEntry{
				Id:            id,
				Kind:          walletdkrpc.EntryKind_ENTRY_KIND_EXIT,
				Status:        walletdkrpc.EntryStatus_ENTRY_STATUS_PENDING,
				CreatedAtUnix: 100,
				UpdatedAtUnix: 100,
			})
	}

	// First update: the live loop receives it and wedges inside Send.
	emit("u1")
	<-stream.entered

	// While the loop is wedged, overflow the one-slot buffer: u2 buffers,
	// u3/u4 cannot enqueue and flag the subscriber overflowed.
	emit("u2")
	emit("u3")
	emit("u4")

	// Release the wedged send; the loop then observes the overflow and
	// signals a gap before continuing.
	close(stream.release)

	require.Eventually(t, func() bool {
		for _, r := range stream.responses() {
			if r.GetGap() != nil {
				return true
			}
		}

		return false
	}, time.Second, 10*time.Millisecond, "overflow must yield a gap signal")

	// The gap carries a resume cursor (the last entry cursor sent before
	// it).
	for _, r := range stream.responses() {
		if r.GetGap() != nil {
			require.Positive(
				t, r.GetCursor(),
				"gap advertises a resume cursor",
			)
		}
	}

	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
}

// TestProjectAndEmitConcurrentProducersMonotonic is the H-1 regression: many
// producers projecting and emitting concurrently must reach a subscriber in
// strictly ascending event_seq order with no drops. Without the project/emit
// serialization a later-committed but lower seq could emit after a higher one,
// and the live cursor would skip it silently.
func TestProjectAndEmitConcurrentProducersMonotonic(t *testing.T) {
	t.Parallel()

	// A large buffer so nothing overflows: this exercises ordering, not the
	// gap path.
	_, _, runtime := newSubscribeFixture(t, 256)
	sub := runtime.subscribe()
	defer runtime.unsubscribe(sub)

	const producers = 50
	var wg sync.WaitGroup
	for i := 0; i < producers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()

			runtime.projectAndEmit(context.Background(),
				&walletdkrpc.WalletEntry{
					Id: fmt.Sprintf("op-%d", i),
					Kind: walletdkrpc.
						EntryKind_ENTRY_KIND_EXIT,
					Status: walletdkrpc.
						EntryStatus_ENTRY_STATUS_PENDING,
					CreatedAtUnix: 100,
					UpdatedAtUnix: 100,
				})
		}(i)
	}
	wg.Wait()

	// Every distinct projection must arrive exactly once, strictly
	// ascending.
	var last int64
	seen := make(map[string]bool)
	for i := 0; i < producers; i++ {
		select {
		case u := <-sub.ch:
			require.Greater(
				t, u.seq, last,
				"emits must arrive in strictly ascending seq",
			)
			last = u.seq
			require.False(
				t, seen[u.entry.GetId()], "duplicate entry %q",
				u.entry.GetId(),
			)
			seen[u.entry.GetId()] = true

		case <-time.After(5 * time.Second):
			t.Fatalf("received only %d of %d concurrent emits", i,
				producers)
		}
	}
	require.Len(t, seen, producers)
}
