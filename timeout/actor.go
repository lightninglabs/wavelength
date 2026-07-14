package timeout

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightningnetwork/lnd/fn/v2"
)

// oneshotEntry tracks a one-shot timeout scheduled via
// ScheduleTimeoutRequest. The generation counter lets a stale fire that
// raced with a Cancel/reschedule identify itself and exit cleanly when
// it lands inside Receive.
type oneshotEntry struct {
	timer    Stoppable
	gen      uint64
	callback actor.TellOnlyRef[*ExpiredMsg]
}

// recurringEntry tracks a recurring tick scheduled via
// ScheduleRecurringTickRequest. interval is preserved so each
// internalTickFired handler can re-arm without re-deriving it from the
// original request.
type recurringEntry struct {
	timer    Stoppable
	gen      uint64
	interval time.Duration
	callback actor.TellOnlyRef[*TickFiredMsg]
}

// Actor is the timeout scheduling actor. It manages one-shot timers and
// recurring tickers, sending notifications when they fire.
//
// All state lives only inside the actor's Receive goroutine. Clock
// callbacks Tell self with an internalTimerFired / internalTickFired
// message and the real state mutation happens when that message reaches
// Receive — there is no cross-goroutine map access, so no mutex.
//
// Recurring ticks are implemented as a chain of one-shot timers: each
// fire delivers TickFiredMsg to the user callback and re-arms via
// AfterFunc. This gives "fixed-delay" semantics (the next tick is
// scheduled at handler-finish + interval) and avoids the burst-of-ticks
// catch-up behaviour of time.Ticker when a consumer is slow.
type Actor struct {
	// clock is the time source. RealClock in production; a fake clock
	// in tests.
	clock Clock

	// self is the actor's reference to its own mailbox. Set by Start
	// after the actor system has registered the behavior. Clock
	// callbacks run in separate goroutines, so Start publishes the ref
	// atomically before any callback reads it.
	self atomic.Value

	// nextGen issues a fresh generation number on every Schedule call.
	// A stale internalTimerFired/internalTickFired arriving after a
	// Cancel or reschedule will see its gen no longer match the live
	// entry and silently drop.
	nextGen uint64

	// oneshots maps timeout IDs to their active one-shot entries.
	oneshots map[ID]*oneshotEntry

	// recurring maps tick IDs to their active recurring entries.
	recurring map[ID]*recurringEntry
}

// NewActor creates a new timeout actor backed by the real wall clock.
// The returned actor must be wired to its mailbox via Start before any
// schedule request is delivered.
func NewActor() *Actor {
	return NewActorWithClock(RealClock{})
}

// NewActorWithClock creates a new timeout actor that uses the supplied
// clock. Tests use this constructor with a fake clock to drive
// scheduling deterministically.
func NewActorWithClock(clock Clock) *Actor {
	return &Actor{
		clock:     clock,
		oneshots:  make(map[ID]*oneshotEntry),
		recurring: make(map[ID]*recurringEntry),
	}
}

// Start attaches the actor's self-reference. Production callers obtain
// this ref from actor.RegisterWithSystem and pass it here so that clock
// callbacks can Tell internal fire messages back into the actor's own
// mailbox. Tests that drive the actor directly (without an
// ActorSystem) inject a synchronous self-ref via newSyncSelfRef.
//
// Start must be called before any ScheduleTimeoutRequest or
// ScheduleRecurringTickRequest is delivered. It is safe to call once
// only — subsequent calls overwrite the self-ref.
func (a *Actor) Start(self actor.TellOnlyRef[Msg]) {
	a.self.Store(self)
}

// loadSelf returns the actor self-reference published by Start. The false case
// is only expected in direct tests or misuse that bypasses actor registration.
func (a *Actor) loadSelf() (actor.TellOnlyRef[Msg], bool) {
	self, ok := a.self.Load().(actor.TellOnlyRef[Msg])

	return self, ok
}

// Receive processes incoming messages.
func (a *Actor) Receive(ctx context.Context, msg Msg) fn.Result[Resp] {
	switch m := msg.(type) {
	case *ScheduleTimeoutRequest:
		return a.handleSchedule(ctx, m)

	case *ScheduleRecurringTickRequest:
		return a.handleScheduleRecurring(ctx, m)

	case *CancelTimeoutRequest:
		return a.handleCancel(ctx, m)

	case *internalTimerFired:
		return a.handleTimerFired(ctx, m)

	case *internalTickFired:
		return a.handleTickFired(ctx, m)

	default:
		return fn.Err[Resp](fmt.Errorf("unknown message type: %T", msg))
	}
}

// handleSchedule schedules a new one-shot timeout. If an entry (one-shot
// or recurring) already exists for this ID, it is cancelled first.
func (a *Actor) handleSchedule(_ context.Context,
	req *ScheduleTimeoutRequest) fn.Result[Resp] {

	a.cancelExisting(req.ID)

	a.nextGen++
	gen := a.nextGen
	id := req.ID

	// AfterFunc fires from the clock's own goroutine; it must not touch
	// any actor state directly. Instead it Tells an internalTimerFired
	// message back into self, where Receive will deliver the
	// user-visible ExpiredMsg single-threadedly.
	//nolint:contextcheck // timer callback outlives scheduling actor turn
	timer := a.clock.AfterFunc(req.Duration, func() {
		self, ok := a.loadSelf()
		if !ok {
			return
		}

		_ = self.Tell(context.Background(), &internalTimerFired{
			ID:  id,
			Gen: gen,
		})
	})

	a.oneshots[id] = &oneshotEntry{
		timer:    timer,
		gen:      gen,
		callback: req.Callback,
	}

	return fn.Ok[Resp](&AckResponse{
		Success: true,
	})
}

// handleScheduleRecurring schedules a recurring tick. If an entry
// (one-shot or recurring) already exists for this ID, it is cancelled
// first. A re-arming chain of AfterFunc one-shots drives the cadence;
// Tick delivery and re-arm both happen inside Receive when
// internalTickFired lands.
//
// Interval must be strictly positive. A zero or negative interval would
// schedule an immediate fire whose handler re-arms with the same value,
// trapping the actor in an unbounded fire/re-arm loop that starves
// every other message in the mailbox. We reject the request before
// touching state so a malformed request cannot disturb existing
// entries.
func (a *Actor) handleScheduleRecurring(_ context.Context,
	req *ScheduleRecurringTickRequest) fn.Result[Resp] {

	if req.Interval <= 0 {
		return fn.Err[Resp](
			fmt.Errorf("recurring tick interval must be "+
				"positive, got %s", req.Interval),
		)
	}

	a.cancelExisting(req.ID)

	a.nextGen++
	gen := a.nextGen

	a.recurring[req.ID] = &recurringEntry{
		gen:      gen,
		interval: req.Interval,
		callback: req.Callback,
	}

	//nolint:contextcheck // recurring timer is owned by timeout actor
	a.armRecurring(req.ID, gen, req.Interval)

	return fn.Ok[Resp](&AckResponse{
		Success: true,
	})
}

// handleCancel cancels a pending one-shot timeout or recurring tick. If
// no entry exists for this ID, this is a no-op.
func (a *Actor) handleCancel(_ context.Context,
	req *CancelTimeoutRequest) fn.Result[Resp] {

	a.cancelExisting(req.ID)

	return fn.Ok[Resp](&AckResponse{
		Success: true,
	})
}

// handleTimerFired delivers a one-shot expiry to its callback. Stale
// fires (cancelled or rescheduled before the timer ran) are dropped via
// the generation check.
func (a *Actor) handleTimerFired(ctx context.Context,
	m *internalTimerFired) fn.Result[Resp] {

	entry, ok := a.oneshots[m.ID]
	if !ok || entry.gen != m.Gen {
		return fn.Ok[Resp](&AckResponse{
			Success: true,
		})
	}

	delete(a.oneshots, m.ID)

	_ = entry.callback.Tell(ctx, &ExpiredMsg{
		ID: m.ID,
	})

	return fn.Ok[Resp](&AckResponse{
		Success: true,
	})
}

// handleTickFired delivers a recurring-tick fire to its callback and
// re-arms the next one-shot. Stale fires (cancelled or replaced before
// the timer ran) are dropped via the generation check.
func (a *Actor) handleTickFired(ctx context.Context,
	m *internalTickFired) fn.Result[Resp] {

	entry, ok := a.recurring[m.ID]
	if !ok || entry.gen != m.Gen {
		return fn.Ok[Resp](&AckResponse{
			Success: true,
		})
	}

	_ = entry.callback.Tell(ctx, &TickFiredMsg{
		ID:      m.ID,
		FiredAt: m.FiredAt,
	})

	//nolint:contextcheck // recurring timer is owned by timeout actor
	a.armRecurring(m.ID, entry.gen, entry.interval)

	return fn.Ok[Resp](&AckResponse{
		Success: true,
	})
}

// armRecurring schedules the next AfterFunc that will fire an
// internalTickFired for (id, gen). The newly created timer handle is
// recorded on the live entry so a subsequent Cancel can stop the
// pending fire. If the entry has been replaced (gen mismatch), the
// timer handle is dropped — the in-flight Tell will be filtered by the
// gen check on arrival.
func (a *Actor) armRecurring(id ID, gen uint64, d time.Duration) {
	timer := a.clock.AfterFunc(d, func() {
		self, ok := a.loadSelf()
		if !ok {
			return
		}

		_ = self.Tell(context.Background(), &internalTickFired{
			ID:      id,
			Gen:     gen,
			FiredAt: a.clock.Now(),
		})
	})

	if entry, ok := a.recurring[id]; ok && entry.gen == gen {
		entry.timer = timer
	} else {
		// Entry was replaced or cancelled between AfterFunc
		// returning and this assignment — stop the dangling
		// timer rather than letting it fire into a dropped gen.
		timer.Stop()
	}
}

// cancelExisting removes any one-shot or recurring entry for id. It runs
// inside Receive so no synchronization is needed for the map ops; the
// timer.Stop() calls are safe across goroutines on their own. Any
// AfterFunc that was already in flight when Stop landed will deliver
// its internal message anyway, but the gen check in
// handleTimerFired/handleTickFired drops it cleanly.
func (a *Actor) cancelExisting(id ID) {
	if entry, ok := a.oneshots[id]; ok {
		entry.timer.Stop()
		delete(a.oneshots, id)
	}

	if entry, ok := a.recurring[id]; ok {
		if entry.timer != nil {
			entry.timer.Stop()
		}
		delete(a.recurring, id)
	}
}
