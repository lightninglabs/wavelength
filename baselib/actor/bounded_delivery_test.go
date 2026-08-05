package actor

import (
	"context"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// TestTellWithoutParkingFailsFastOnFullMailbox verifies the property the helper
// exists for: a caller that must stay live gets ErrMailboxFull back from a
// bounded target instead of parking on it. The contrast with Tell is what the
// wedge was made of — Tell on this same reference never returns while the
// target stays parked.
func TestTellWithoutParkingFailsFastOnFullMailbox(t *testing.T) {
	t.Parallel()

	h := newActorTestHarness(t)
	a, _ := wedgedActor(t, h)

	start := time.Now()
	outsideTx, err := TellWithoutParking[*testMsg](
		t.Context(), a.Ref(), newTestMsg("overflow"),
	).Unpack()

	require.ErrorIs(t, err, ErrMailboxFull)

	// A refused hand-off delivered nothing, so nothing escaped the
	// caller's transaction and there is nothing to suppress on a retry.
	require.False(t, outsideTx)

	// The bound is loose for the same reason as the TryTell tests: a loaded
	// CI box can deschedule us mid-call. It still catches the regression,
	// because a blocking send here would never return at all.
	require.Less(t, time.Since(start), time.Second)
}

// TestTellWithoutParkingDelivers verifies the success path is ordinary
// delivery.
func TestTellWithoutParkingDelivers(t *testing.T) {
	t.Parallel()

	h := newActorTestHarness(t)
	beh := newEchoBehavior(t, 0)
	a := h.newActor("tell-without-parking-delivers", beh, 4)

	replies := make(chan string, 1)
	msg := newTestMsgWithReply("payload", replies)

	outsideTx, err := TellWithoutParking[*testMsg](
		t.Context(), a.Ref(), msg,
	).Unpack()
	require.NoError(t, err)

	// The reported flag is what a caller inside a transaction reads to know
	// the hand-off is NOT covered by its commit, so it has to be true for a
	// bounded in-memory target.
	require.True(t, outsideTx)

	select {
	case got := <-replies:
		require.Equal(t, "payload", got)

	case <-time.After(time.Second):
		t.Fatal("message was never processed")
	}
}

// TestTellWithoutParkingClassifiesByTarget pins the classification by
// observing which primitive actually runs for each reference shape that
// reaches the helper from the ingress path, rather than by asking a
// capability question. The durable direction is the expensive one to get
// wrong — a durable enqueue swapped onto TryTell leaves the caller's
// transaction behind, so it can survive a rollback.
func TestTellWithoutParkingClassifiesByTarget(t *testing.T) {
	t.Parallel()

	h := newActorTestHarness(t)

	// A full bounded target is the discriminator: only the TryTell path can
	// report ErrMailboxFull, and only the Tell path could park. Every
	// transform wrapper must route a bounded target through TryTell.
	// FilterMapInputRef is the one that was missing the delegation: a
	// wrapper that does not answer falls back to the blocking Tell path and
	// reinstates the wedge with no deferral metric and no log.
	full, _ := wedgedActor(t, h)

	mapInput := NewMapInputRef(
		TellOnlyRef[*testMsg](
			full.Ref(),
		),
		func(m *testMsg) *testMsg { return m },
	)
	filterMap := NewFilterMapInputRef(
		TellOnlyRef[*testMsg](
			full.Ref(),
		),
		func(m *testMsg) (*testMsg, bool) { return m, true },
	)
	mapRef := NewMapRef(
		ActorRef[*testMsg, string](
			full.Ref(),
		),
		func(m *testMsg) (*testMsg, error) { return m, nil },
		func(r string) string { return r },
	)

	wrappers := map[string]TellOnlyRef[*testMsg]{
		"direct":            full.Ref(),
		"map-input":         mapInput,
		"filter-map":        filterMap,
		"map-bidirectional": mapRef,
	}
	for name, ref := range wrappers {
		outsideTx, err := TellWithoutParking[*testMsg](
			t.Context(), ref, newTestMsg("classify"),
		).Unpack()
		// ErrMailboxFull is itself the classification: only the
		// TryTell path can report it, and the Tell path would have
		// parked instead. The refused hand-off delivered nothing, so
		// the result carries false.
		require.ErrorIs(
			t, err, ErrMailboxFull, "%s: a full bounded target "+
				"must surface ErrMailboxFull, not park or "+
				"deliver", name,
		)
		require.False(t, outsideTx, name)
	}

	// A reference this package does not recognise keeps the plain Tell:
	// that is the conservative default that protects a durable enqueue's
	// transaction atomicity.
	unknown := &unboundedTestRef{}
	outsideTx, err := TellWithoutParking[*testMsg](
		t.Context(), unknown, newTestMsg("conservative"),
	).Unpack()
	require.NoError(t, err)
	require.False(t, outsideTx)
	require.Equal(t, 1, unknown.tells)
	require.Zero(t, unknown.tryTells)

	// A router with nothing registered resolves no target and falls back to
	// its own Tell, which reports the empty key immediately instead of
	// parking.
	system := NewActorSystem()
	t.Cleanup(func() {
		require.NoError(t, system.Shutdown(context.Background()))
	})

	key := NewServiceKey[*testMsg, string]("bounded-classification-key")
	router := key.Ref(system)

	start := time.Now()
	err = TellWithoutParking[*testMsg](
		t.Context(), router, newTestMsg("empty-key"),
	).Err()
	require.Error(t, err)
	require.Less(t, time.Since(start), time.Second)
}

// unboundedTestRef stands in for a reference whose enqueue is a database write
// rather than a fixed-capacity mailbox: it is not a NonParkingTeller, the way
// an externally implemented reference would not be, and records which
// primitive was used on it. The real durable ref cannot be used here because
// its TryTell needs a store, and the point being pinned is the choice of
// primitive, not the write.
type unboundedTestRef struct {
	tells    int
	tryTells int
}

// Tell records a blocking enqueue.
func (r *unboundedTestRef) Tell(context.Context, *testMsg) error {
	r.tells++

	return nil
}

// TryTell records a non-blocking enqueue, which for a durable target is the
// wrong primitive: it drops the caller's transaction.
func (r *unboundedTestRef) TryTell(context.Context, *testMsg) error {
	r.tryTells++

	return nil
}

// Ask is unused by these tests and completes immediately.
func (r *unboundedTestRef) Ask(context.Context, *testMsg) Future[string] {
	promise := NewPromise[string]()
	promise.Complete(fn.Ok(""))

	return promise.Future()
}

// ID identifies the double.
func (r *unboundedTestRef) ID() string {
	return "unbounded-test-ref"
}

// TestTellWithoutParkingResolvesRouterTarget pins the coherence between the
// delivery decision and the target it sends to.
//
// A Router's send picks ONE target round-robin, while its key can carry both
// a bounded actor and a durable one at once. Deciding the primitive from any
// set-wide answer puts roughly half of the deliveries through the durable
// ref's TryTell — which abandons the caller's transaction, does its write on
// a 100ms background context that cannot complete while the caller holds a
// single-writer database, and comes back as a wrapped DeadlineExceeded rather
// than ErrMailboxFull. The ingress loop would read that as a hard dispatch
// failure, roll back, never advance, and report no deferral: total deafness
// with the wrong diagnosis. So the primitive has to be chosen against the
// resolved target, not the set.
func TestTellWithoutParkingResolvesRouterTarget(t *testing.T) {
	t.Parallel()

	h := newActorTestHarness(t)

	system := NewActorSystem()
	t.Cleanup(func() {
		require.NoError(t, system.Shutdown(context.Background()))
	})

	key := NewServiceKey[*testMsg, string]("mixed-registration-key")

	// One bounded actor and one unbounded reference under the same key. The
	// receptionist accepts this: registration is an append-only slice and
	// only the message and response type names are checked.
	bounded := h.newActor("mixed-bounded", newEchoBehavior(t, 0), 4)
	unbounded := &unboundedTestRef{}

	require.NoError(
		t,
		RegisterWithReceptionist(
			system.Receptionist(), key, bounded.Ref(),
		),
	)
	require.NoError(
		t,
		RegisterWithReceptionist(
			system.Receptionist(), key, unbounded,
		),
	)

	router := key.Ref(system)

	// Two sends cover both targets, whichever order round-robin visits them
	// in. Neither may reach the unbounded reference through TryTell.
	for range 2 {
		require.NoError(
			t, TellWithoutParking[*testMsg](
				t.Context(), router, newTestMsg("mixed"),
			).Err(),
		)
	}

	require.Zero(
		t, unbounded.tryTells,
		"a durable target was delivered to with TryTell",
	)
	require.Equal(t, 1, unbounded.tells)

	// The same has to hold through a transform in front of the router,
	// since that is how several ingress routes are wired.
	wrapped := NewMapInputRef(
		TellOnlyRef[*testMsg](router),
		func(m *testMsg) *testMsg { return m },
	)

	for range 2 {
		require.NoError(
			t, TellWithoutParking[*testMsg](
				t.Context(), wrapped, newTestMsg(
					"mixed-wrapped",
				),
			).Err(),
		)
	}

	require.Zero(
		t, unbounded.tryTells, "a durable target behind a "+
			"transform was delivered to with TryTell",
	)
	require.Equal(t, 2, unbounded.tells)
}
