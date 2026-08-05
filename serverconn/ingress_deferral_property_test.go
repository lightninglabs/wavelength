package serverconn

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/lightninglabs/wavelength/baselib/actor"
	mailboxpb "github.com/lightninglabs/wavelength/mailbox/pb"
	mailboxrpc "github.com/lightninglabs/wavelength/mailbox/rpc"
	"github.com/lightninglabs/wavelength/serverconn/hellotestpb"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"pgregory.net/rapid"
)

// drawAscendingEventSeqs draws a strictly ascending event_seq run starting at
// or after 1, the way a mailbox stamps a pulled window. Gaps are allowed
// because a pull filters by recipient, so a client's window is ascending but
// not necessarily dense.
func drawAscendingEventSeqs(rt *rapid.T, maxCount int) []uint64 {
	count := rapid.IntRange(0, maxCount).Draw(rt, "envelope_count")

	seqs := make([]uint64, 0, count)

	seq := uint64(0)
	for range count {
		seq += uint64(rapid.IntRange(1, 3).Draw(rt, "seq_gap"))
		seqs = append(seqs, seq)
	}

	return seqs
}

// TestClampToDeferred_CursorSafety_Property pins the clamp that keeps a redrive
// from repeating a previous cycle's pre-transaction work.
//
// The clamp is on the cursor's critical path: it returns both the batch the
// fold will process and the exclusive cursor that batch commits, so a clamp
// that returned a cursor covering an envelope it dropped would ack an event no
// dispatcher ever saw. The state space is the cross product of batch shape,
// gap layout and where in (or outside) the batch the deferral fell, which is
// exactly the shape example-based tests sample thinly.
func TestClampToDeferred_CursorSafety_Property(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		seqs := drawAscendingEventSeqs(rt, 8)

		envelopes := make([]*mailboxpb.Envelope, 0, len(seqs))
		input := make(map[uint64]bool, len(seqs))
		for _, seq := range seqs {
			envelopes = append(envelopes, eventEnvelope(seq))
			input[seq] = true
		}

		var last uint64
		if len(seqs) > 0 {
			last = seqs[len(seqs)-1]
		}

		// The mailbox returns the exclusive cursor one past the last
		// envelope it handed over; the slack covers a server that
		// skipped trailing envelopes the client cannot see.
		slack := rapid.IntRange(0, 2).Draw(rt, "cursor_slack")
		nextCursor := last + 1 + uint64(slack)

		// The deferral is drawn from inside the batch, from a gap, and
		// from past its end, which is the case where the envelope that
		// wedged the last cycle is no longer on the mailbox.
		deferredSeq := rapid.Uint64Range(0, last+2).Draw(
			rt, "deferred_seq",
		)

		clamped, cursor := clampToDeferred(
			envelopes, nextCursor, deferredSeq,
		)

		// The clamp only ever drops envelopes: it never reorders the
		// batch and never invents an envelope that was not pulled.
		kept := make(map[uint64]bool, len(clamped))
		var prevSeq uint64
		for i, env := range clamped {
			if i > 0 && env.EventSeq <= prevSeq {
				rt.Fatalf("clamp reordered event_seq %d "+
					"after %d", env.EventSeq, prevSeq)
			}
			if !input[env.EventSeq] {
				rt.Fatalf("clamp invented event_seq %d",
					env.EventSeq)
			}

			prevSeq = env.EventSeq
			kept[env.EventSeq] = true
		}

		// CURSOR SAFETY: an exclusive cursor acks every event_seq below
		// it, so nothing the clamp dropped may sit below the cursor it
		// returned. This is the one arithmetic here that can ack an
		// envelope no dispatcher will ever see.
		for _, env := range envelopes {
			if kept[env.EventSeq] {
				continue
			}

			if env.EventSeq < cursor {
				rt.Fatalf("clamp cursor %d covers dropped "+
					"event_seq %d", cursor, env.EventSeq)
			}
		}

		// The converse keeps the clamp useful rather than merely safe:
		// the cursor has to cover everything the clamp kept, or the
		// caller could not commit the batch it was handed.
		for _, env := range clamped {
			if env.EventSeq >= cursor {
				rt.Fatalf("clamp cursor %d does not cover "+
					"kept event_seq %d", cursor,
					env.EventSeq)
			}
		}

		// The clamp narrows the window; it never widens it past what
		// the pull actually returned.
		if cursor > nextCursor {
			rt.Fatalf("clamp cursor %d ran past pull cursor %d",
				cursor, nextCursor)
		}

		// A cleared deferral is the identity, which is what an
		// unwedged loop runs on every batch.
		if deferredSeq == 0 && (len(clamped) != len(envelopes) ||
			cursor != nextCursor) {

			rt.Fatalf("clamp with no deferral changed the batch")
		}

		// Re-clamping is a fixed point. A wedged target is redriven
		// with the same deferredSeq for as long as it stays wedged, so
		// a clamp that kept narrowing would walk the cursor backwards
		// over cycles.
		again, againCursor := clampToDeferred(
			clamped, cursor, deferredSeq,
		)
		if len(again) != len(clamped) || againCursor != cursor {
			rt.Fatalf("clamp is not idempotent: %d/%d then %d/%d",
				len(clamped), cursor, len(again), againCursor)
		}
	})
}

// TestDeferredCursor_NeverAcksDeferred_Property pins the cursor the fold
// commits when a full mailbox stops a batch partway.
//
// deferredCursor is four lines, and all four are load-bearing: it is the only
// place the loop turns "this envelope was refused" into "everything before it
// is acked". Off by one in either direction is a bug with no local symptom —
// one too high silently drops a round event, one too low replays a hoisted
// request on every redrive of the episode.
func TestDeferredCursor_NeverAcksDeferred_Property(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		pullCursor := rapid.Uint64Range(0, 64).Draw(rt, "pull_cursor")
		deferredSeq := rapid.Uint64Range(0, 64).Draw(
			rt, "deferred_seq",
		)

		cursor := deferredCursor(deferredSeq, pullCursor)

		// The cursor never goes backwards, so a re-pull of already
		// committed envelopes cannot rewind it.
		if cursor < pullCursor {
			rt.Fatalf("deferred cursor %d rewound past %d", cursor,
				pullCursor)
		}

		// It is always one of its two inputs: the function either holds
		// the cursor where it was or moves it exactly onto the
		// undelivered envelope. Anything else would be arithmetic
		// nobody can bound.
		if cursor != pullCursor && cursor != deferredSeq {
			rt.Fatalf("deferred cursor %d is neither %d nor %d",
				cursor, pullCursor, deferredSeq)
		}

		// CURSOR SAFETY: the deferred envelope was refused by its
		// target, so it must never fall below the committed cursor.
		// The carve-out is a cursor that was already past it, where an
		// earlier cycle committed the envelope and nothing can be
		// un-acked.
		if deferredSeq >= pullCursor && cursor > deferredSeq {
			rt.Fatalf("deferred cursor %d acked undelivered "+
				"event_seq %d", cursor, deferredSeq)
		}

		// A zero event_seq is never trusted as a cursor: the mailbox
		// stamps from 1, so a zero means the envelope was never stamped
		// by a server.
		if deferredSeq == 0 && cursor != pullCursor {
			rt.Fatalf("zero deferredSeq moved the cursor to %d",
				cursor)
		}
	})
}

// TestSplitAndMerge_PreserveOrder_Property pins the per-target ordering
// contract of the two functions that reshape a pulled batch before it is
// dispatched. splitIngressEnvelopes fans the batch into three delivery paths
// and mergeEnvelopesByEventSeq folds the vanished-waiter stragglers back into
// the durable one; between them they must lose nothing, duplicate nothing, and
// leave every partition in event_seq order, because the durable partition's
// order is the order the target actor's lane sees.
func TestSplitAndMerge_PreserveOrder_Property(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		seqs := drawAscendingEventSeqs(rt, 8)

		// Each envelope independently rolls whether it is a response
		// with a live waiter, a hoisted request, or an ordinary durable
		// event, so the partition is exercised in every mixture.
		envelopes := make([]*mailboxpb.Envelope, 0, len(seqs))
		waiters := make(map[uint64]bool, len(seqs))
		hoisted := make(map[uint64]bool, len(seqs))
		for _, seq := range seqs {
			kind := rapid.IntRange(0, 2).Draw(rt, "kind")

			switch kind {
			case 0:
				corrID := "corr-" + strconv.FormatUint(seq, 10)
				envelopes = append(
					envelopes,
					responseEnvelope(corrID, seq),
				)
				waiters[seq] = rapid.Bool().Draw(
					rt, "waiter_live",
				)

			case 1:
				envelopes = append(
					envelopes, requestEnvelope(
						deferPropHoistedService,
						deferPropHoistedMethod, seq,
					),
				)
				hoisted[seq] = true

			default:
				envelopes = append(
					envelopes, eventEnvelope(seq),
				)
			}
		}

		live := make(map[CorrelationID]bool, len(waiters))
		for seq, ok := range waiters {
			corrID := "corr-" + strconv.FormatUint(seq, 10)
			live[CorrelationID(corrID)] = ok
		}

		hasWaiter := func(id CorrelationID) bool {
			return live[id]
		}
		isNonTx := func(env *mailboxpb.Envelope) bool {
			return hoisted[env.EventSeq]
		}

		responses, nonTx, durables := splitIngressEnvelopes(
			envelopes, hasWaiter, isNonTx,
		)

		// The three partitions cover the batch exactly once each.
		total := len(responses) + len(nonTx) + len(durables)
		if total != len(envelopes) {
			rt.Fatalf("split changed the batch size: %d -> %d",
				len(envelopes), total)
		}

		seen := make(map[uint64]int, len(envelopes))
		for _, part := range [][]*mailboxpb.Envelope{
			responses, nonTx, durables,
		} {

			requireAscending(rt, part)

			for _, env := range part {
				seen[env.EventSeq]++
			}
		}
		for _, seq := range seqs {
			if seen[seq] != 1 {
				rt.Fatalf("event_seq %d appears in %d "+
					"partitions", seq, seen[seq])
			}
		}

		// Any subset of the responses can come back as a straggler when
		// its waiter vanishes between the split peek and the delivery.
		// Folding them back has to restore a single ascending lane.
		var stragglers []*mailboxpb.Envelope
		for _, env := range responses {
			if rapid.Bool().Draw(rt, "waiter_vanished") {
				stragglers = append(stragglers, env)
			}
		}

		merged := mergeEnvelopesByEventSeq(durables, stragglers)
		requireAscending(rt, merged)

		if len(merged) != len(durables)+len(stragglers) {
			rt.Fatalf("merge changed the batch size: %d + %d -> %d",
				len(durables), len(stragglers), len(merged))
		}
	})
}

// requireAscending fails the property when a partition is not in ascending
// event_seq order.
func requireAscending(rt *rapid.T, envelopes []*mailboxpb.Envelope) {
	for i := 1; i < len(envelopes); i++ {
		if envelopes[i].EventSeq <= envelopes[i-1].EventSeq {
			rt.Fatalf("partition out of order at %d: %d after %d",
				i, envelopes[i].EventSeq,
				envelopes[i-1].EventSeq)
		}
	}
}

// deferralBackoffSlack is how much a measured sleep may overshoot its ceiling
// before the property fails. The assertion is a ceiling, not a stopwatch: a
// loaded CI box can deschedule the test goroutine for a while after the timer
// fires, and only a bound that ignores that noise is worth having.
const deferralBackoffSlack = 250 * time.Millisecond

// TestSleepDeferralBackoff_BoundedDelay_Property pins the redrive schedule's
// ceiling.
//
// The redrive is the only thing that moves a wedged client's inbound backlog,
// so its delay is bounded by maxDeferralDelay rather than by the transport's
// RetryMaxDelay: at the transport ceiling a recovered target drains one pull
// window per 30s, which is slower than the round client's own deadlines. A
// deployment that configures something TIGHTER than the backpressure cap keeps
// it, so the bound is the minimum of the two, and both directions have to hold
// for every schedule rather than for the two the example tests pick.
func TestSleepDeferralBackoff_BoundedDelay_Property(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		// Most draws use a millisecond-scale schedule, where the
		// configured ceiling is the binding one and a full sleep costs
		// nothing. A saturated draw is the only way to show that
		// maxDeferralDelay binds a schedule whose own ceiling is far
		// above it, and showing that means sleeping most of it, so it
		// is drawn rarely.
		saturated := rapid.IntRange(0, 15).Draw(rt, "saturated") == 0

		baseMs := rapid.IntRange(1, 8).Draw(rt, "base_ms")
		maxMs := rapid.IntRange(1, 20).Draw(rt, "max_ms")
		attempt := rapid.IntRange(0, 4).Draw(rt, "attempt")

		cfg := newTestConnectorConfig(
			newInMemoryMailbox(), newMemCheckpointStore(),
		)
		cfg.RetryBaseDelay = time.Duration(baseMs) * time.Millisecond
		cfg.RetryMaxDelay = time.Duration(maxMs) * time.Millisecond

		if saturated {
			// A base above the ceiling and a transport ceiling far
			// above the backpressure cap: nothing but
			// maxDeferralDelay can bound this sleep.
			cfg.RetryBaseDelay = 5 * time.Second
			cfg.RetryMaxDelay = 30 * time.Second
		}

		conn := NewServerConnectionActor(cfg)

		ceiling := cfg.RetryMaxDelay
		if ceiling <= 0 || ceiling > maxDeferralDelay {
			ceiling = maxDeferralDelay
		}

		failCount := attempt

		start := time.Now()
		conn.sleepDeferralBackoff(context.Background(), &failCount)
		elapsed := time.Since(start)

		if elapsed > ceiling+deferralBackoffSlack {
			rt.Fatalf("deferral backoff slept %s with base %s and "+
				"max %s, ceiling is %s", elapsed,
				cfg.RetryBaseDelay, cfg.RetryMaxDelay, ceiling)
		}

		// The schedule advances by exactly one attempt per redrive.
		// The count is the loop's own, kept apart from the transport
		// one, and a redrive that skipped or double-counted would
		// silently move the whole episode onto a different curve.
		if failCount != attempt+1 {
			rt.Fatalf("deferral fail count went %d -> %d", attempt,
				failCount)
		}
	})
}

// Property-test routes. Each names one of the three ways an inbound envelope
// reaches its destination, so a generated batch can mix all of them in one
// pull window.
const (
	// deferPropService and deferPropMethod name the route whose target is
	// a real actor behind a real fixed-capacity mailbox. This is the route
	// that defers.
	deferPropService = "deferprop.v1.HelloService"
	deferPropMethod  = "HelloStarted"

	// deferPropHoistedService and deferPropHoistedMethod name a marked
	// non-transactional route, whose dispatcher stands in for the mux
	// bridge: it answers the operator over the network and enqueues
	// nothing.
	deferPropHoistedService = "deferprop.v1.DaemonService"
	deferPropHoistedMethod  = "GetInfo"

	// deferPropUnaryService and deferPropUnaryMethod name the route the
	// waiter-backed responses arrive on. It has no dispatcher, because a
	// response with a live waiter never reaches the dispatch table.
	deferPropUnaryService = "deferprop.v1.UnaryService"
	deferPropUnaryMethod  = "Echo"
)

// errDeferPropServe is the injected failure of a hoisted request's round trip
// to the operator, which is the one error that can stop a folded dispatch
// before its transaction ever opens.
var errDeferPropServe = errors.New("deferprop: send RPC response failed")

// deferPropKind is how a generated envelope is meant to be delivered.
type deferPropKind uint8

const (
	// deferPropDurable is a KIND_EVENT for the in-memory target actor,
	// dispatched inside the folded write transaction.
	deferPropDurable deferPropKind = iota

	// deferPropHoisted is a KIND_REQUEST on a marked route, served before
	// the transaction opens.
	deferPropHoisted

	// deferPropWaiter is a KIND_RESPONSE with a live in-memory waiter,
	// delivered before the transaction opens.
	deferPropWaiter
)

// gatedBehavior is a real actor behavior whose receive turn the test holds
// shut. It stands in for any of the unbounded waits a round client actor can
// park on, and unlike stalledBehavior its gate can be reopened and shut again,
// which is what lets one property run toggle the target's mailbox between full
// and drained across redrive cycles.
type gatedBehavior struct {
	mu sync.Mutex

	// openCh is closed while the gate is open. A shut gate replaces it
	// with a fresh open channel, so a receive turn that has not started
	// yet blocks on the new one.
	openCh chan struct{}

	// delivered records the session ID of every processed message, in
	// processing order, so the test can compare the target's lane against
	// the deliveries the fold actually made.
	delivered []string
}

// newGatedBehavior returns a behavior whose gate starts shut.
func newGatedBehavior() *gatedBehavior {
	return &gatedBehavior{
		openCh: make(chan struct{}),
	}
}

// Receive parks until the gate opens, then records the message. Cancellation
// is honoured so the actor system can shut down while the gate is still shut.
func (b *gatedBehavior) Receive(ctx context.Context,
	msg *helloStartedMsg) fn.Result[struct{}] {

	b.mu.Lock()
	gate := b.openCh
	b.mu.Unlock()

	select {
	case <-gate:
	case <-ctx.Done():
		return fn.Err[struct{}](ctx.Err())
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	b.delivered = append(b.delivered, msg.SessionID)

	return fn.Ok(struct{}{})
}

// open lets the target drain.
func (b *gatedBehavior) open() {
	b.mu.Lock()
	defer b.mu.Unlock()

	select {
	case <-b.openCh:
	default:
		close(b.openCh)
	}
}

// shut wedges the target again, so the next redrive meets a mailbox that fills
// up.
func (b *gatedBehavior) shut() {
	b.mu.Lock()
	defer b.mu.Unlock()

	select {
	case <-b.openCh:
		b.openCh = make(chan struct{})

	default:
	}
}

// count returns how many messages the target has processed.
func (b *gatedBehavior) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()

	return len(b.delivered)
}

// observed returns a copy of the processed session IDs.
func (b *gatedBehavior) observed() []string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return append([]string(nil), b.delivered...)
}

// deferPropHarness is a connector wired the way the daemon wires its round
// route — a real actor system, a real fixed-capacity mailbox, a real
// EventRouter route and a store that models the single global writer — plus
// the bookkeeping a property run needs to hold the fold to its contract.
//
// Nothing on the path under test is faked. The two dispatchers the harness
// installs wrap or stand beside the real ones and only OBSERVE what they did,
// so the model the invariants are checked against is built out of the
// production pipeline's own outcomes rather than out of a second
// implementation of it.
type deferPropHarness struct {
	conn     *ServerConnectionActor
	system   *actor.ActorSystem
	store    *writerLockStore
	behavior *gatedBehavior

	// envelopes is the generated window, in ascending event_seq order, and
	// kinds says how each of them is meant to be delivered.
	envelopes []*mailboxpb.Envelope
	kinds     map[uint64]deferPropKind

	// waiters holds one live in-memory waiter per waiter-backed response.
	waiters map[uint64]actor.Future[*mailboxpb.Envelope]

	// cycle is the index of the redrive cycle currently running. It is
	// what turns "delivered twice" into the two questions that have
	// different answers: twice inside one folded dispatch is the
	// deliveredOutsideTx regression, twice across cycles is documented
	// at-least-once behaviour.
	cycle int

	// failHoisted makes every hoisted serve in the current cycle fail.
	failHoisted bool

	// durableAccepted records the envelopes whose dispatcher returned nil,
	// meaning the target's mailbox took them.
	durableAccepted map[uint64]bool

	// durableTellCycle is the last cycle in which an envelope was actually
	// handed to the mailbox rather than suppressed as an in-flight
	// replay.
	durableTellCycle map[uint64]int

	// expectedLog is the session IDs the target must process, in the order
	// the fold handed them over. expectedSeq and expectedCycle carry the
	// event_seq and the cycle of each of those hand-offs.
	expectedLog   []string
	expectedSeq   []uint64
	expectedCycle []int

	// hoistedServed counts the SUCCESSFUL serves of each hoisted request,
	// which is the count a redrive must not multiply.
	hoistedServed map[uint64]int

	// dupInFold records an envelope handed to the mailbox twice inside one
	// folded dispatch.
	dupInFold []uint64

	// deferrals counts the redrives a full mailbox turned away, so the
	// test can prove the run was not vacuous.
	deferrals int
}

// deferPropCorrID is the correlation ID of the waiter-backed response at
// event_seq seq.
func deferPropCorrID(seq uint64) CorrelationID {
	return CorrelationID("deferprop-" + strconv.FormatUint(seq, 10))
}

// deferPropEventEnvelope builds the KIND_EVENT envelope for the in-memory
// target's route. The session ID is the event_seq, so the target's processed
// lane can be compared against the deliveries the fold made.
func deferPropEventEnvelope(rt *rapid.T, seq uint64) *mailboxpb.Envelope {
	body, err := anypb.New(&hellotestpb.HelloStartedEvent{
		SessionId: strconv.FormatUint(seq, 10),
	})
	require.NoError(rt, err)

	env := nonTxTestEnvelope(
		mailboxpb.RpcMeta_KIND_EVENT, deferPropService, deferPropMethod,
		seq,
	)
	env.Recipient = "client-1"
	env.Body = body

	return env
}

// drawDeferPropBatch draws a pulled window of mixed durable, hoisted and
// waiter-backed envelopes with ascending event_seqs.
func drawDeferPropBatch(rt *rapid.T) ([]*mailboxpb.Envelope,
	map[uint64]deferPropKind) {

	count := rapid.IntRange(1, 10).Draw(rt, "envelope_count")

	envelopes := make([]*mailboxpb.Envelope, 0, count)
	kinds := make(map[uint64]deferPropKind, count)

	// Durable events are drawn twice as often as the other two kinds. They
	// are the only ones a full mailbox can turn away, so a window that is
	// mostly hoisted requests never reaches the deferral this is about.
	kindWeights := []deferPropKind{
		deferPropDurable, deferPropDurable, deferPropHoisted,
		deferPropWaiter,
	}

	seq := uint64(0)
	for range count {
		seq += uint64(rapid.IntRange(1, 2).Draw(rt, "seq_gap"))

		kind := rapid.SampledFrom(kindWeights).Draw(rt, "kind")
		kinds[seq] = kind

		switch kind {
		case deferPropHoisted:
			env := nonTxTestEnvelope(
				mailboxpb.RpcMeta_KIND_REQUEST,
				deferPropHoistedService, deferPropHoistedMethod,
				seq,
			)
			env.Recipient = "client-1"
			envelopes = append(envelopes, env)

		case deferPropWaiter:
			env := nonTxTestEnvelope(
				mailboxpb.RpcMeta_KIND_RESPONSE,
				deferPropUnaryService, deferPropUnaryMethod,
				seq,
			)
			env.Recipient = "client-1"
			env.Rpc.CorrelationId = string(deferPropCorrID(seq))
			envelopes = append(envelopes, env)

		case deferPropDurable:
			envelopes = append(
				envelopes, deferPropEventEnvelope(rt, seq),
			)
		}
	}

	return envelopes, kinds
}

// newDeferPropHarness builds the connector, spawns the gated target behind a
// real mailbox of the given capacity, and registers a live waiter for every
// waiter-backed response in the window.
func newDeferPropHarness(rt *rapid.T, capacity int,
	envelopes []*mailboxpb.Envelope,
	kinds map[uint64]deferPropKind) *deferPropHarness {

	h := &deferPropHarness{
		system: actor.NewActorSystemWithConfig(actor.SystemConfig{
			MailboxCapacity: capacity,
		}),
		store:     newWriterLockStore(),
		behavior:  newGatedBehavior(),
		envelopes: envelopes,
		kinds:     kinds,
		waiters: make(
			map[uint64]actor.Future[*mailboxpb.Envelope],
		),
		durableAccepted:  make(map[uint64]bool),
		durableTellCycle: make(map[uint64]int),
		hoistedServed:    make(map[uint64]int),
	}

	// Register the target through the ordinary path, so it gets a genuine
	// ChannelMailbox at the configured capacity rather than a test double:
	// a mocked reference is exactly what would make this vacuous, because
	// the backpressure being modelled lives in the mailbox.
	key := actor.NewServiceKey[*helloStartedMsg, struct{}](deferPropService)
	key.Spawn(h.system, "gated-target", h.behavior)

	router := NewEventRouter(h.system)
	AddRoute(router, EventRouteConfig[*helloStartedMsg, struct{}]{
		Service: deferPropService,
		Method:  deferPropMethod,
		NewEvent: func() proto.Message {
			return &hellotestpb.HelloStartedEvent{}
		},
		Key: key,
		Adapt: func(p proto.Message) (*helloStartedMsg, error) {
			m := &helloStartedMsg{}

			return m, m.FromProto(p)
		},
	})

	cfg := newTestConnectorConfig(newInMemoryMailbox(), nil)
	cfg.MailboxProtocolVersion = 1
	cfg.ArkProtocolVersion = 1
	cfg.Store = h.store
	cfg.Dispatchers = router.AsDispatcherMap()

	durableRoute := mailboxrpc.ServiceMethod{
		Service: deferPropService,
		Method:  deferPropMethod,
	}
	cfg.Dispatchers[durableRoute] = h.observeDurable(
		cfg.Dispatchers[durableRoute],
	)

	hoistedRoute := mailboxrpc.ServiceMethod{
		Service: deferPropHoistedService,
		Method:  deferPropHoistedMethod,
	}
	cfg.Dispatchers[hoistedRoute] = h.serveHoisted
	cfg.NonTxRoutes = RouteSet{
		hoistedRoute: {},
	}

	h.conn = NewServerConnectionActor(cfg)

	for seq, kind := range kinds {
		if kind != deferPropWaiter {
			continue
		}

		h.waiters[seq] = h.conn.RegisterWaiter(deferPropCorrID(seq))
	}

	return h
}

// stop releases the target and shuts the actor system down.
func (h *deferPropHarness) stop() {
	// The gate is opened first: a parked receive turn would otherwise make
	// the shutdown wait for the very target the run wedged.
	h.behavior.open()

	shutdownCtx, cancel := context.WithTimeout(
		context.Background(), 5*time.Second,
	)
	defer cancel()

	_ = h.system.Shutdown(shutdownCtx)
}

// observeDurable wraps the router's real dispatcher and records what it did.
// The wrapper adds no behaviour of its own: the delivery, the mailbox and the
// deferral all come from the production path.
func (h *deferPropHarness) observeDurable(
	inner EnvelopeDispatcher) EnvelopeDispatcher {

	return func(ctx context.Context, env *mailboxpb.Envelope) error {
		// Ask the fold's in-flight record what the real dispatcher is
		// about to do. A seen envelope is one this folded dispatch
		// already handed to the mailbox, so deliverToActor
		// short-circuits and no second copy reaches the target. Reading
		// it here is what lets the model tell a suppressed replay apart
		// from a genuine hand-off — and a record that stopped
		// suppressing would show up as a duplicate below rather than
		// being quietly absorbed.
		suppressed := deliveredOutsideTxFrom(ctx).seen(env.EventSeq)

		if err := inner(ctx, env); err != nil {
			return err
		}

		h.durableAccepted[env.EventSeq] = true

		if suppressed {
			return nil
		}

		if last, ok := h.durableTellCycle[env.EventSeq]; ok &&
			last == h.cycle {

			h.dupInFold = append(h.dupInFold, env.EventSeq)
		}

		h.durableTellCycle[env.EventSeq] = h.cycle
		h.expectedLog = append(
			h.expectedLog, strconv.FormatUint(env.EventSeq, 10),
		)
		h.expectedSeq = append(h.expectedSeq, env.EventSeq)
		h.expectedCycle = append(h.expectedCycle, h.cycle)

		return nil
	}
}

// serveHoisted stands in for the mux bridge: it answers the operator over the
// network and enqueues nothing, so a redrive that ran it twice would send a
// second response to a request that has already been answered.
func (h *deferPropHarness) serveHoisted(_ context.Context,
	env *mailboxpb.Envelope) error {

	if h.failHoisted {
		return errDeferPropServe
	}

	h.hoistedServed[env.EventSeq]++

	return nil
}

// handled reports whether the envelope at seq has reached its destination:
// queued in the target's mailbox, answered over the network, or handed to its
// in-memory waiter. This is the set the committed cursor is checked against.
func (h *deferPropHarness) handled(seq uint64) bool {
	switch h.kinds[seq] {
	case deferPropHoisted:
		return h.hoistedServed[seq] > 0

	case deferPropWaiter:
		return futureIsDone(h.waiters[seq])

	case deferPropDurable:
		return h.durableAccepted[seq]
	}

	return false
}

// futureIsDone reports whether a response waiter's future has already been
// completed, without blocking on it. Await answers from its result cache
// before it ever looks at the context, so a cancelled one turns the blocking
// read into a poll.
func futureIsDone(future actor.Future[*mailboxpb.Envelope]) bool {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	return future.Await(ctx).IsOk()
}

// snapshotHandled captures the handled set before a cycle runs, so the cycle's
// own progress can be told apart from what earlier cycles had already done.
func (h *deferPropHarness) snapshotHandled() map[uint64]bool {
	snapshot := make(map[uint64]bool, len(h.envelopes))
	for _, env := range h.envelopes {
		snapshot[env.EventSeq] = h.handled(env.EventSeq)
	}

	return snapshot
}

// window returns the envelopes a pull at the current cursor would hand back,
// along with the exclusive next cursor, matching what the mailbox computes.
func (h *deferPropHarness) window(state AckState) ([]*mailboxpb.Envelope,
	uint64) {

	var (
		batch  []*mailboxpb.Envelope
		maxSeq uint64
	)
	for _, env := range h.envelopes {
		if env.EventSeq < state.PullCursor {
			continue
		}

		batch = append(batch, env)
		maxSeq = env.EventSeq
	}

	if len(batch) == 0 {
		return nil, state.PullCursor
	}

	return batch, maxSeq + 1
}

// drainTarget lets the wedged target empty its mailbox and then shuts the gate
// again, so the next redrive starts against a mailbox with room. Waiting for
// the target to catch up with the deliveries the fold made is what keeps the
// toggle deterministic: every cycle starts with the target either fully
// drained or holding exactly what earlier cycles gave it.
func (h *deferPropHarness) drainTarget(rt *rapid.T) {
	h.behavior.open()

	want := len(h.expectedLog)
	deadline := time.Now().Add(10 * time.Second)
	for h.behavior.count() < want {
		if time.Now().After(deadline) {
			rt.Fatalf("target drained %d of %d deliveries",
				h.behavior.count(), want)
		}

		time.Sleep(100 * time.Microsecond)
	}

	h.behavior.shut()
}

// runCycle runs one redrive of the pull window through the real folded
// dispatch and checks the invariants the cycle has to preserve. It mirrors the
// loop control ingressLoop applies around runFoldedDispatch — adopt the
// advanced state on a deferral, hold the old one on a failure, clear the
// deferral on success — and nothing else. It returns false when the window is
// empty and there is nothing left to redrive.
//
// When chaos is set the cycle also injects the two faults a real one can meet:
// a store that replays its transaction body after a retryable error, and a
// hoisted serve whose round trip to the operator fails.
func (h *deferPropHarness) runCycle(rt *rapid.T, state *AckState,
	redrive *redriveState, chaos bool) bool {

	batch, nextCursor := h.window(*state)
	if len(batch) == 0 {
		return false
	}

	h.cycle++
	h.dupInFold = nil
	h.failHoisted = false

	replays := 0
	if chaos {
		if rapid.Bool().Draw(rt, "drain_target") {
			h.drainTarget(rt)
		}

		replays = rapid.IntRange(0, 2).Draw(rt, "store_replays")
		h.failHoisted = rapid.IntRange(0, 5).Draw(
			rt, "serve_fails",
		) == 0
	}

	store := &replayingStore{
		writerLockStore: h.store,
		replays:         replays,
	}

	before := h.snapshotHandled()
	prev := *state

	newState, err := h.conn.runFoldedDispatch(
		context.Background(), store, batch, nextCursor, prev, redrive,
	)

	var deferral *deferredDispatchError
	switch {
	case errors.As(err, &deferral):
		h.deferrals++

		*state = newState
		redrive.deferredSeq = deferral.eventSeq

	case err != nil:
		// A hoisted serve that failed returns before the fold, so the
		// cursor stays where it was and the batch is re-pulled whole.
		if newState != prev {
			rt.Fatalf("failed dispatch moved the state: %+v -> %+v",
				prev, newState)
		}

	default:
		*state = newState
		redrive.deferredSeq = 0
	}

	h.checkCycle(rt, prev, *state, nextCursor, deferral, before)

	return true
}

// checkCycle holds one redrive to the invariants that must survive any mixture
// of deferral point, store replay and mailbox state.
func (h *deferPropHarness) checkCycle(rt *rapid.T, prev, state AckState,
	nextCursor uint64, deferral *deferredDispatchError,
	before map[uint64]bool) {

	// CURSOR SAFETY. The committed cursor is exclusive, so every envelope
	// below it has been acknowledged; if one of those never reached its
	// destination it is gone, and on this path that means a silently
	// dropped round event. The handled set is built from what the real
	// dispatchers returned, so this compares the cursor against the
	// pipeline's own outcomes.
	for _, env := range h.envelopes {
		if env.EventSeq >= state.PullCursor {
			continue
		}

		if !h.handled(env.EventSeq) {
			rt.Fatalf("cursor %d acked unhandled event_seq %d "+
				"(kind %d)", state.PullCursor, env.EventSeq,
				h.kinds[env.EventSeq])
		}
	}

	// A transaction body that ran more than once must not hand the same
	// envelope to a bounded in-memory mailbox more than once: that
	// delivery is not in the transaction and a rollback does not take it
	// back.
	if len(h.dupInFold) > 0 {
		rt.Fatalf("event_seqs %v delivered twice inside one folded "+
			"dispatch", h.dupInFold)
	}

	// The cursor is monotonic and never runs past the window the pull
	// returned. A store that replayed its body cannot compound the
	// advance, because the fold re-derives both outputs from the caller's
	// state on every attempt.
	if state.PullCursor < prev.PullCursor {
		rt.Fatalf("cursor regressed: %d -> %d", prev.PullCursor,
			state.PullCursor)
	}
	if state.PullCursor > nextCursor {
		rt.Fatalf("cursor %d ran past the pulled window %d",
			state.PullCursor, nextCursor)
	}

	// A hoisted request is answered over the network, so serving it twice
	// sends the operator a second response to a request it has already had
	// answered. Once per episode, however many redrives the episode takes.
	for seq, served := range h.hoistedServed {
		if served > 1 {
			rt.Fatalf("hoisted event_seq %d served %d times", seq,
				served)
		}
	}

	if deferral == nil {
		return
	}

	// The cursor a deferral commits is exactly what deferredCursor says it
	// should be, which ties the fold to the pure function the property
	// above pins.
	want := deferredCursor(deferral.eventSeq, prev.PullCursor)
	if state.PullCursor != want {
		rt.Fatalf("deferred cycle committed %d, want %d",
			state.PullCursor, want)
	}

	// A redrive that got part of the backlog through has to move the
	// cursor. This is the signal the loop keys deferFailCount's reset on:
	// without an advance to observe, a draining target is redriven at the
	// backoff ceiling instead of promptly, and backpressure that has
	// already cleared still fails rounds.
	for _, env := range h.envelopes {
		if env.EventSeq >= deferral.eventSeq || before[env.EventSeq] {
			continue
		}

		if !h.handled(env.EventSeq) {
			continue
		}

		if state.PullCursor <= prev.PullCursor {
			rt.Fatalf("redrive handled event_seq %d but left the "+
				"cursor at %d", env.EventSeq, state.PullCursor)
		}
	}
}

// TestFoldedDispatch_DeferralInvariants_Property drives the real folded
// dispatch through randomized backpressure episodes and holds it to the
// invariants a deferral must not break.
//
// The reason this is a property test rather than another example is the size
// of the state space. An episode is a cross product of where in the batch the
// deferral falls, how many times the store replays its transaction body, which
// hoisted serves fail, and whether the target drained between redrives — and
// the invariants are about what SURVIVES all of that, not about any one path
// through it. The example tests in ingress_backpressure_test.go each pin one
// point in that space; this pins the space.
//
// Everything under test is production code: a real actor system, a real
// fixed-capacity mailbox, the real EventRouter dispatcher, and
// runFoldedDispatch itself. The harness only observes what those returned and
// compares the committed cursor against it.
func TestFoldedDispatch_DeferralInvariants_Property(t *testing.T) {
	t.Parallel()

	// Deferrals are counted across the whole run so a change that stopped
	// producing backpressure — a bigger default mailbox, a generator that
	// drifted — cannot turn this into a test that passes by never
	// exercising the path.
	var deferrals int

	rapid.Check(t, func(rt *rapid.T) {
		envelopes, kinds := drawDeferPropBatch(rt)

		// A small mailbox only makes the wedge cheap to reach: the
		// backpressure it produces is capacity-independent, and
		// production runs the same code at the default 100.
		capacity := rapid.IntRange(1, 3).Draw(rt, "mailbox_capacity")

		h := newDeferPropHarness(rt, capacity, envelopes, kinds)
		defer h.stop()

		var (
			state   AckState
			redrive redriveState
		)

		// Phase one is the episode: redrives against a target that is
		// mostly wedged, with faults injected.
		cycles := rapid.IntRange(0, 2*len(envelopes)+2).Draw(
			rt, "chaos_cycles",
		)
		for range cycles {
			if !h.runCycle(rt, &state, &redrive, true) {
				break
			}
		}

		// Phase two is the recovery: the target drains before every
		// redrive and no faults are injected, so the backlog has to
		// clear. Each cycle starts against an empty mailbox and
		// therefore delivers at least one more envelope, which bounds
		// the loop.
		for range 2*len(envelopes) + 8 {
			h.drainTarget(rt)

			if !h.runCycle(rt, &state, &redrive, false) {
				break
			}
		}

		if batch, _ := h.window(state); len(batch) > 0 {
			rt.Fatalf("%d envelopes still unprocessed at cursor %d",
				len(batch), state.PullCursor)
		}

		// NO LOSS. Every envelope the window carried reached its
		// destination once the mailboxes drained.
		for _, env := range h.envelopes {
			if !h.handled(env.EventSeq) {
				rt.Fatalf("event_seq %d (kind %d) was "+
					"never handled", env.EventSeq,
					h.kinds[env.EventSeq])
			}
		}

		// NO REPLAY. Each hoisted request was answered exactly once,
		// however many redrives the episode took.
		for seq, kind := range h.kinds {
			if kind != deferPropHoisted {
				continue
			}

			if h.hoistedServed[seq] != 1 {
				rt.Fatalf("hoisted event_seq %d served "+
					"%d times", seq, h.hoistedServed[seq])
			}
		}

		// ORDERING. Within a cycle the fold hands envelopes over in
		// event_seq order; a duplicate can only come from a later
		// cycle re-driving a window whose cursor never moved.
		for i := 1; i < len(h.expectedSeq); i++ {
			if h.expectedCycle[i] != h.expectedCycle[i-1] {
				continue
			}

			if h.expectedSeq[i] <= h.expectedSeq[i-1] {
				rt.Fatalf("cycle %d delivered %d after %d",
					h.expectedCycle[i], h.expectedSeq[i],
					h.expectedSeq[i-1])
			}
		}

		// The target's own lane is the ground truth for both ordering
		// and duplication: it must hold exactly the deliveries the fold
		// made, in the order the fold made them.
		h.behavior.open()

		deadline := time.Now().Add(10 * time.Second)
		for h.behavior.count() < len(h.expectedLog) {
			if time.Now().After(deadline) {
				break
			}

			time.Sleep(100 * time.Microsecond)
		}

		if got := h.behavior.observed(); !slices.Equal(
			got, h.expectedLog,
		) {

			rt.Fatalf("target processed %v, fold delivered %v", got,
				h.expectedLog)
		}

		deferrals += h.deferrals
	})

	require.Positive(
		t, deferrals, "no redrive was ever deferred, so the "+
			"generated batches never wedged the target",
	)
}
