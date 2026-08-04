package serverconn

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lightninglabs/wavelength/baselib/actor"
	mailboxpb "github.com/lightninglabs/wavelength/mailbox/pb"
	mailboxrpc "github.com/lightninglabs/wavelength/mailbox/rpc"
	"github.com/lightninglabs/wavelength/metrics"
	"github.com/lightninglabs/wavelength/serverconn/hellotestpb"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
)

const (
	// stormMailboxCapacity is the target actor's mailbox capacity. It is
	// small only so the test is fast: the wedge it reproduces is
	// capacity-independent, and production runs the same code with the
	// default 100.
	stormMailboxCapacity = 4

	// stormEnvelopes is how many events the storm pushes. It has to exceed
	// the mailbox capacity by enough that the dispatch loop is still
	// holding undelivered envelopes when it gives up.
	stormEnvelopes = 12
)

// stalledBehavior is a real actor behavior that parks its receive turn until
// the test releases it, then records every message it processes. It stands in
// for a round client actor parked on any of the many unbounded waits it has: an
// FSM transition, an un-deadlined Ask into the wallet or chain source, or a
// state query on a machine mid-transition.
type stalledBehavior struct {
	// release gates the first receive turn. Closing it lets the actor
	// drain.
	release chan struct{}

	mu sync.Mutex

	// delivered records the session ID of every processed message, in
	// processing order, so the test can assert exactly-once delivery.
	delivered []string
}

// newStalledBehavior returns a behavior that parks until released.
func newStalledBehavior() *stalledBehavior {
	return &stalledBehavior{
		release: make(chan struct{}),
	}
}

// Receive parks until the test releases the actor, then records the message.
func (b *stalledBehavior) Receive(ctx context.Context,
	msg *helloStartedMsg) fn.Result[struct{}] {

	select {
	case <-b.release:
	case <-ctx.Done():
		return fn.Err[struct{}](ctx.Err())
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	b.delivered = append(b.delivered, msg.SessionID)

	return fn.Ok(struct{}{})
}

// observed returns a copy of the processed session IDs.
func (b *stalledBehavior) observed() []string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return append([]string(nil), b.delivered...)
}

// stormConfig describes the traffic a storm harness seeds and the schedule its
// ingress loop runs on. The zero value is the tight-schedule wedge the original
// regression test uses.
type stormConfig struct {
	// route is the service name of the wedged target. It is unique per test
	// so the deferral counter assertion reads only this test's label
	// series.
	route string

	// events is how many KIND_EVENT envelopes to seed on that route.
	events int

	// hoistedAfter seeds one hoisted KIND_REQUEST per entry, after that
	// many wedged events. The target accepts stormMailboxCapacity+1 events
	// before it defers (its mailbox plus the one held in the parked receive
	// turn), so an entry at or below that lands the request's event_seq
	// BELOW the envelope that defers and an entry above it lands ABOVE.
	// Both sides have to be covered: they were re-served by different
	// arithmetic.
	hoistedAfter []int

	// retryBaseDelay and retryMaxDelay override the connector's backoff.
	// Zero leaves the millisecond-scale schedule that keeps the wedge tests
	// fast; a test that asserts on RECOVERY timing has to set production
	// values, because a 10ms ceiling hides the schedule entirely.
	retryBaseDelay time.Duration
	retryMaxDelay  time.Duration
}

// stormHarness is a connector wired the way the daemon wires its round route: a
// real actor system, a real fixed-capacity mailbox, a real EventRouter route,
// and a store that models the single global database writer. Nothing on the
// edge under test is faked, because a mocked actor reference is exactly what
// would make this test vacuous — the bug lives in the mailbox.
type stormHarness struct {
	conn     *ServerConnectionActor
	store    *writerLockStore
	mailbox  *inMemoryMailbox
	behavior *stalledBehavior

	// service and method name the wedged route.
	service string
	method  string

	// hoistedService and hoistedMethod name a marked non-transactional
	// route, whose dispatcher stands in for the mux bridge: it answers the
	// operator over the network rather than enqueuing anything.
	hoistedService string
	hoistedMethod  string

	// hoistedServes counts how many times that dispatcher ran, which is the
	// count a redrive must not multiply.
	hoistedServes atomic.Int64
}

// newStormHarness builds the connector, registers the stalled actor, and seeds
// the remote mailbox with count events on the actor's route.
func newStormHarness(t *testing.T, route string, count int) *stormHarness {
	t.Helper()

	return newStormHarnessWith(t, stormConfig{
		route:  route,
		events: count,
	})
}

// newStormHarnessWith is newStormHarness with the traffic and schedule spelled
// out.
func newStormHarnessWith(t *testing.T, cfg stormConfig) *stormHarness {
	t.Helper()

	system := actor.NewActorSystemWithConfig(actor.SystemConfig{
		MailboxCapacity: stormMailboxCapacity,
	})
	t.Cleanup(func() {
		require.NoError(t, system.Shutdown(context.Background()))
	})

	h := &stormHarness{
		store:          newWriterLockStore(),
		mailbox:        newInMemoryMailbox(),
		behavior:       newStalledBehavior(),
		service:        cfg.route,
		method:         "HelloStarted",
		hoistedService: cfg.route + ".Hoisted",
		hoistedMethod:  "GetInfo",
	}

	// Register the target through the ordinary path, so it gets a genuine
	// ChannelMailbox at the configured capacity rather than a test double.
	key := actor.NewServiceKey[*helloStartedMsg, struct{}](cfg.route)
	key.Spawn(system, "stalled-target", h.behavior)

	router := NewEventRouter(system)
	AddRoute(router, EventRouteConfig[*helloStartedMsg, struct{}]{
		Service: h.service,
		Method:  h.method,
		NewEvent: func() proto.Message {
			return &hellotestpb.HelloStartedEvent{}
		},
		Key: key,
		Adapt: func(p proto.Message) (*helloStartedMsg, error) {
			m := &helloStartedMsg{}

			return m, m.FromProto(p)
		},
	})

	connCfg := newTestConnectorConfig(h.mailbox, nil)
	connCfg.MailboxProtocolVersion = 1
	connCfg.ArkProtocolVersion = 1
	connCfg.Store = h.store
	connCfg.Dispatchers = router.AsDispatcherMap()

	// Wire the hoisted route by hand, the way the daemon marks its
	// mux-bridged routes: a dispatcher that does IO and no enqueue, plus
	// the NonTxRoutes mark that pulls it out of the fold.
	hoistedRoute := mailboxrpc.ServiceMethod{
		Service: h.hoistedService,
		Method:  h.hoistedMethod,
	}
	connCfg.Dispatchers[hoistedRoute] = func(context.Context,
		*mailboxpb.Envelope) error {

		h.hoistedServes.Add(1)

		return nil
	}
	connCfg.NonTxRoutes = RouteSet{
		hoistedRoute: {},
	}

	// Keep the retry schedule tight by default: the loop is expected to
	// back off on every deferral, and a wedge test should not wait out a
	// production backoff to observe the next attempt.
	connCfg.RetryBaseDelay = time.Millisecond
	connCfg.RetryMaxDelay = 10 * time.Millisecond
	if cfg.retryBaseDelay > 0 {
		connCfg.RetryBaseDelay = cfg.retryBaseDelay
	}
	if cfg.retryMaxDelay > 0 {
		connCfg.RetryMaxDelay = cfg.retryMaxDelay
	}

	h.conn = NewServerConnectionActor(connCfg)

	for i := range cfg.events {
		for _, at := range cfg.hoistedAfter {
			if at == i {
				h.seedHoisted(t)
			}
		}

		h.seed(t, fmt.Sprintf("s-%d", i))
	}

	for _, at := range cfg.hoistedAfter {
		if at >= cfg.events {
			h.seedHoisted(t)
		}
	}

	return h
}

// eventEnvelope builds one KIND_EVENT envelope for the harness route. The
// mailbox assigns the event_seq on send; a direct dispatch caller sets seq
// itself.
func (h *stormHarness) eventEnvelope(t *testing.T, sessionID string,
	seq uint64) *mailboxpb.Envelope {

	t.Helper()

	body, err := anypb.New(&hellotestpb.HelloStartedEvent{
		SessionId: sessionID,
	})
	require.NoError(t, err)

	env := nonTxTestEnvelope(
		mailboxpb.RpcMeta_KIND_EVENT, h.service, h.method, seq,
	)
	env.Recipient = "client-1"
	env.Body = body

	return env
}

// seed pushes one KIND_EVENT envelope for the harness route onto the client's
// remote mailbox.
func (h *stormHarness) seed(t *testing.T, sessionID string) {
	t.Helper()

	status := h.mailbox.send(h.eventEnvelope(t, sessionID, 0))
	require.True(t, status.Ok)
}

// seedHoisted pushes one KIND_REQUEST envelope for the marked route onto the
// client's remote mailbox.
func (h *stormHarness) seedHoisted(t *testing.T) {
	t.Helper()

	env := nonTxTestEnvelope(
		mailboxpb.RpcMeta_KIND_REQUEST, h.hoistedService,
		h.hoistedMethod, 0,
	)
	env.Recipient = "client-1"

	status := h.mailbox.send(env)
	require.True(t, status.Ok)
}

// run starts the ingress loop and returns a cancel func plus a channel that
// closes when the loop has returned.
func (h *stormHarness) run(t *testing.T) (context.CancelFunc, chan struct{}) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	h.conn.wg.Add(1)
	go func() {
		defer close(done)

		h.conn.ingressLoop(ctx, AckState{})
	}()

	t.Cleanup(func() {
		cancel()

		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("ingress loop did not exit")
		}
	})

	return cancel, done
}

// deferrals reads this harness route's deferral counter.
func (h *stormHarness) deferrals() float64 {
	return testutil.ToFloat64(
		metrics.ServerConnIngressDeferredTotal.WithLabelValues(
			h.service, h.method,
		),
	)
}

// TestIngressStormDoesNotWedgeOnFullRoundMailbox is the regression test for the
// wedge that made a deployed client go silently deaf to round events.
//
// The ingress loop is the process's only consumer of the server mailbox, and it
// used to deliver into the round client's fixed-capacity in-memory mailbox with
// a blocking Tell, on a context with no deadline, from inside the folded write
// transaction. A target that stopped draining therefore parked that one
// goroutine forever with the database's single writer held, while read-only
// RPCs kept answering — a healthy-looking pod that had stopped hearing the
// operator.
//
// The test asserts the properties rather than timing them: the writer lock must
// not be held while the target is stalled, the loop must keep taking turns, no
// envelope may be acknowledged before it is delivered, and every envelope must
// still arrive exactly once after the target drains.
func TestIngressStormDoesNotWedgeOnFullRoundMailbox(t *testing.T) {
	t.Parallel()

	h := newStormHarness(t, "storm.v1.HelloService", stormEnvelopes)
	_, _ = h.run(t)

	// The loop must reach a state where it has stopped dispatching but has
	// NOT parked: every transaction it opened has closed, and the modeled
	// global writer is free. Before the fix the last thing in the log is a
	// tx-open with no matching tx-close, because the loop is parked inside
	// ChannelMailbox.Send with the writer held.
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		observed := h.store.observed()
		if !assert.NotEmpty(c, observed) {
			return
		}

		assert.Equal(
			c, "tx-close", observed[len(observed)-1],
			"loop is parked inside the write transaction",
		)
		assert.False(
			c, h.store.writerHeld(),
			"the modeled global writer is still held",
		)
	}, 5*time.Second, 10*time.Millisecond)

	// Backpressure has to be visible. This is the observability half of the
	// fix: the old path's only log was a mailbox-level Trace, which is why
	// two incidents produced no evidence.
	require.Eventually(t, func() bool {
		return h.deferrals() > 0
	}, 5*time.Second, 10*time.Millisecond,
		"no dispatch deferral was recorded")

	// The ack watermark must stop short of the envelopes that were never
	// handed over. Acking past an undelivered event is how backpressure
	// would turn into a lost round event, and it is what leaves nothing for
	// the redelivery below to find.
	//
	// The most the mailbox can have taken is its capacity, plus the one
	// message the stalled actor is holding in its receive turn; the cursor
	// is exclusive, so one more than that. Sequence numbers are dense from
	// 1.
	const maxAcked = stormMailboxCapacity + 2

	acked := h.mailbox.getAckedUpTo("client-1")
	require.LessOrEqual(t, acked, uint64(maxAcked))
	require.Less(t, acked, uint64(stormEnvelopes+1))

	// Release the target. Everything the storm pushed must arrive, exactly
	// once and in order, including the envelopes that were deferred.
	close(h.behavior.release)

	want := make([]string, 0, stormEnvelopes)
	for i := range stormEnvelopes {
		want = append(want, fmt.Sprintf("s-%d", i))
	}

	require.EventuallyWithT(t, func(c *assert.CollectT) {
		assert.Len(
			c, h.behavior.observed(), stormEnvelopes,
			"deferred envelopes were never redelivered",
		)
	}, 10*time.Second, 10*time.Millisecond)

	require.Equal(t, want, h.behavior.observed())
}

// TestIngressRedriveDoesNotReplayHoistedRequests pins the cost of the redrive
// itself.
//
// Redrive-in-place is now the steady state of a wedged target, and the two
// pre-transaction steps of a folded dispatch — waiter delivery, and the hoisted
// KIND_REQUEST round trips — run over the whole pulled batch before the
// transaction opens. The cursor stops at the undelivered envelope, so without a
// clamp every hoisted request in the pull window is served again on every
// backoff cycle, forever: a duplicate local serve plus a duplicate
// KIND_RESPONSE to the operator, starting exactly when the client is already in
// trouble. The routes queued to be hoisted next (WalletService.SignVTXO,
// RoundService.SubmitNonces) are ones where a duplicate is a second signature,
// not a wasted read.
//
// The assertion is a count, not a timing: however many redrives the wedge
// produces, each hoisted request is served at most once, and exactly once by
// the time the backlog drains.
func TestIngressRedriveDoesNotReplayHoistedRequests(t *testing.T) {
	t.Parallel()

	// One request lands below the envelope that defers and one above it.
	// The two were re-served by different arithmetic: the one below stayed
	// inside the pull window because the cursor stopped one past the last
	// DELIVERED envelope rather than at the deferred one, and the one above
	// because the pre-transaction pass never looked at where the cursor had
	// stopped.
	const hoistedRequests = 2

	h := newStormHarnessWith(t, stormConfig{
		route:  "storm-hoist.v1.HelloService",
		events: stormEnvelopes,
		hoistedAfter: []int{
			stormMailboxCapacity + 1, stormEnvelopes,
		},
	})
	_, _ = h.run(t)

	// Wait for a number of redrives well above the number of hoisted
	// requests, so a per-cycle replay could not hide inside the bound
	// below.
	const redrives = 10

	require.Eventually(t, func() bool {
		return h.deferrals() >= redrives
	}, 10*time.Second, 10*time.Millisecond,
		"the wedge did not produce enough redrives to be conclusive")

	require.LessOrEqual(
		t, h.hoistedServes.Load(), int64(hoistedRequests),
		"a hoisted request was served more than once while the "+
			"target was wedged",
	)

	// Draining must not change the accounting either: every hoisted request
	// is served, and none of them twice.
	close(h.behavior.release)

	require.EventuallyWithT(t, func(c *assert.CollectT) {
		assert.Len(c, h.behavior.observed(), stormEnvelopes)
	}, 10*time.Second, 10*time.Millisecond)

	require.Equal(
		t, int64(hoistedRequests), h.hoistedServes.Load(),
		"hoisted requests were not served exactly once each",
	)
}

// TestIngressBackpressureRecoversPromptly pins the redrive CADENCE, which the
// other tests in this file deliberately cannot see: they run on a 1ms/10ms
// schedule so the wedge is fast to reproduce, and that override hides the whole
// question of how long recovery takes.
//
// This one runs on the production schedule. The failure it guards against is
// backpressure sharing the transport backoff: the deferral path increments the
// same fail count on every re-pull and, without a reset on progress, saturates
// at RetryMaxDelay (30s) and stays there. A recovered target then drains at one
// pull window per 30s, which is slower than the round client's own 60s
// registration and 90s reconcile deadlines — so backpressure that has already
// cleared still fails rounds, after forfeit signatures are on the wire.
//
// The trigger is a COUNT of redrives, not a wall-clock rate, so the assertion
// does not encode a schedule of its own: the point is what happens to recovery
// after the schedule has had time to ratchet, whenever that is. (A rate
// assertion would also be blunted by ackPhase, which resets the shared fail
// count on any cycle with an ack outstanding, so the pre-fix ramp only takes
// hold once the cursor stops advancing — which is exactly the steady state of a
// wedge.)
func TestIngressBackpressureRecoversPromptly(t *testing.T) {
	t.Parallel()

	defaults := DefaultConnectorConfig()

	h := newStormHarnessWith(t, stormConfig{
		route:          "storm-recover.v1.HelloService",
		events:         stormEnvelopes,
		retryBaseDelay: defaults.RetryBaseDelay,
		retryMaxDelay:  defaults.RetryMaxDelay,
	})
	_, _ = h.run(t)

	// Enough redrives that an uncapped doubling schedule is at its 30s
	// ceiling by now. The window is generous because the schedule under
	// test is the thing being measured, not this wait.
	const redrives = 10

	require.Eventually(t, func() bool {
		return h.deferrals() >= redrives
	}, time.Minute, 10*time.Millisecond,
		"the wedge did not produce enough redrives to be conclusive")

	// The target recovers. Everything still queued has to arrive within a
	// few redrives, not within a few transport ceilings. At the 30s ceiling
	// the very next redrive alone overruns this window.
	close(h.behavior.release)

	require.EventuallyWithT(t, func(c *assert.CollectT) {
		assert.Len(
			c, h.behavior.observed(), stormEnvelopes, "the "+
				"backlog did not drain promptly after the "+
				"target recovered",
		)
	}, 5*time.Second, 20*time.Millisecond)
}

// replayingStore models the production store's response to a retryable
// transaction error: it rolls the attempt back and runs the whole body again,
// up to ten times. Whatever the body wrote is discarded by the rollback;
// whatever it did OUTSIDE the transaction is not, which is the asymmetry this
// test is about.
type replayingStore struct {
	*writerLockStore

	// replays is how many rolled-back attempts to run before the one that
	// commits.
	replays int
}

// ExecTx runs the body replays+1 times, each in its own modeled transaction.
func (s *replayingStore) ExecTx(ctx context.Context, readOnly bool,
	fn actor.TxFunc) error {

	for range s.replays {
		if err := s.writerLockStore.ExecTx(
			ctx, readOnly, fn,
		); err != nil {
			return err
		}
	}

	return s.writerLockStore.ExecTx(ctx, readOnly, fn)
}

// TestFoldedDispatchDoesNotReplayInMemoryDeliveries pins the one effect inside
// the fold that a rolled-back transaction does not undo.
//
// A durable enqueue is a write in the fold's transaction, so replaying the body
// after a retryable error (SQLITE_BUSY, Postgres 40001) is a no-op in net
// effect. A delivery into a bounded in-memory mailbox is not in the transaction
// at all: the target actor can have processed the message before the
// transaction even tries to commit. Every attempt therefore used to re-deliver
// the whole durable partition's in-memory half, up to ten times, and then
// report success — no log, no metric, no way to tell from the outside.
func TestFoldedDispatchDoesNotReplayInMemoryDeliveries(t *testing.T) {
	t.Parallel()

	const events = 3

	h := newStormHarnessWith(t, stormConfig{
		route:  "storm-replay.v1.HelloService",
		events: 0,
	})

	// The target drains normally here. The property under test is how many
	// times the fold hands a message over, not what happens when it will
	// not take one.
	close(h.behavior.release)

	store := &replayingStore{
		writerLockStore: h.store,
		replays:         2,
	}

	envelopes := make([]*mailboxpb.Envelope, 0, events)
	for i := range events {
		envelopes = append(
			envelopes,
			h.eventEnvelope(
				t, fmt.Sprintf("r-%d", i), uint64(i+1),
			),
		)
	}

	newState, err := h.conn.runFoldedDispatch(
		t.Context(), store, envelopes, events+1, AckState{},
		&redriveState{},
	)
	require.NoError(t, err)
	require.Equal(t, uint64(events+1), newState.PullCursor)

	want := []string{"r-0", "r-1", "r-2"}

	require.EventuallyWithT(t, func(c *assert.CollectT) {
		assert.Equal(c, want, h.behavior.observed())
	}, 5*time.Second, 10*time.Millisecond)

	// Three replays of a three-envelope batch would be nine deliveries. The
	// mailbox is draining, so any duplicate would show up here rather than
	// being lost to a full mailbox.
	require.Never(t, func() bool {
		return len(h.behavior.observed()) > events
	}, 300*time.Millisecond, 20*time.Millisecond)
}

// TestIngressCancelReleasesStalledDispatch pins the shutdown escape while a
// target is stalled. The loop must return on context cancellation even though
// the target has never drained a single message, and it must do so without
// waiting for the target: a shutdown that hangs on a wedged actor is the same
// unbounded wait in a different place.
func TestIngressCancelReleasesStalledDispatch(t *testing.T) {
	t.Parallel()

	h := newStormHarness(t, "storm-cancel.v1.HelloService", stormEnvelopes)
	cancel, done := h.run(t)

	// Wait until the loop has actually met the full mailbox, so the
	// cancellation lands on a loop that is mid-backpressure rather than one
	// that has not started yet.
	require.Eventually(t, func() bool {
		return h.deferrals() > 0
	}, 5*time.Second, 10*time.Millisecond,
		"no dispatch deferral was recorded")

	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal(
			"ingress loop did not exit while the target was " +
				"stalled",
		)
	}
}
