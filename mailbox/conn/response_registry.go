package conn

import (
	"fmt"
	"sync"
	"time"

	"github.com/lightninglabs/wavelength/baselib/actor"
	mailboxpb "github.com/lightninglabs/wavelength/mailbox/pb"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"google.golang.org/protobuf/proto"
)

// DefaultResponseWaiterTTL bounds stale waiter and buffered response retention
// when no explicit TTL is configured.
const DefaultResponseWaiterTTL = 10 * time.Minute

// ErrWaiterExpired is returned when a response waiter is pruned due to TTL
// expiration while an AwaitRPC caller is still blocked.
var ErrWaiterExpired = fmt.Errorf("response waiter expired")

// ErrWaiterCancelled is returned when a response waiter is explicitly removed
// before a response arrives.
var ErrWaiterCancelled = fmt.Errorf("response waiter cancelled")

// responseWaiter captures the promise and creation time for a single
// correlation ID.
type responseWaiter struct {
	// Promise is completed when a response arrives, the waiter is
	// pruned, or the waiter is explicitly removed.
	Promise actor.Promise[*mailboxpb.Envelope]

	// Created records waiter registration time for stale cleanup.
	Created time.Time
}

// bufferedResponse keeps a cloned envelope until a waiter is registered.
type bufferedResponse struct {
	Envelope *mailboxpb.Envelope
	Created  time.Time
}

// ResponseRegistry tracks correlation waiters and early responses. Waiters
// use actor.Future for context-aware blocking with automatic error signaling
// on TTL expiry.
type ResponseRegistry struct {
	mu sync.Mutex

	waiters   map[CorrelationID]*responseWaiter
	pending   map[CorrelationID]*bufferedResponse
	waiterTTL time.Duration
}

// DeliveryResult reports how DeliverResponse handled a response envelope.
type DeliveryResult uint8

const (
	// DeliveryDropped indicates the response could not be stored or
	// delivered.
	DeliveryDropped DeliveryResult = iota

	// DeliveryWaiter indicates the response completed an active waiter.
	DeliveryWaiter

	// DeliveryBuffered indicates the response was buffered because
	// no waiter was registered yet.
	DeliveryBuffered
)

// String returns a human-readable label for the delivery result.
func (d DeliveryResult) String() string {
	switch d {
	case DeliveryDropped:
		return "dropped"

	case DeliveryWaiter:
		return "waiter"

	case DeliveryBuffered:
		return "buffered"

	default:
		return "unknown"
	}
}

// NewResponseRegistry constructs a response registry with stale-state cleanup.
func NewResponseRegistry(waiterTTL time.Duration) *ResponseRegistry {
	if waiterTTL <= 0 {
		waiterTTL = DefaultResponseWaiterTTL
	}

	return &ResponseRegistry{
		waiters:   make(map[CorrelationID]*responseWaiter),
		pending:   make(map[CorrelationID]*bufferedResponse),
		waiterTTL: waiterTTL,
	}
}

// RegisterWaiter registers or reuses a waiter for correlation ID id. Returns
// an actor.Future that completes when the response arrives, the waiter
// expires, or the waiter is explicitly removed.
func (r *ResponseRegistry) RegisterWaiter(
	id CorrelationID,
) actor.Future[*mailboxpb.Envelope] {

	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()
	r.pruneStaleLocked(now)

	waiter, ok := r.waiters[id]
	if !ok {
		promise := actor.NewPromise[*mailboxpb.Envelope]()
		waiter = &responseWaiter{
			Promise: promise,
			Created: now,
		}

		r.waiters[id] = waiter
	}

	// If a response arrived before the waiter was registered, complete
	// the promise immediately with the buffered envelope.
	if pending, ok := r.pending[id]; ok {
		waiter.Promise.Complete(fn.Ok(pending.Envelope))

		delete(r.pending, id)
	}

	return waiter.Promise.Future()
}

// RemoveWaiter removes an existing waiter for correlation ID id. Any
// goroutine blocked on the associated Future receives ErrWaiterCancelled.
func (r *ResponseRegistry) RemoveWaiter(id CorrelationID) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if waiter, ok := r.waiters[id]; ok {
		waiter.Promise.Complete(
			fn.Err[*mailboxpb.Envelope](ErrWaiterCancelled),
		)

		delete(r.waiters, id)
	}
}

// HasWaiter reports whether an active in-memory waiter is currently registered
// for correlation ID id. It lets the ingress loop classify a KIND_RESPONSE at
// split time: a response with a live waiter can be delivered on the fast
// pre-transaction path, while a response without one must fold into the durable
// dispatch transaction so its enqueue commits atomically with the cursor. Stale
// waiters are pruned before the check so an expired entry never masquerades as
// a live one.
func (r *ResponseRegistry) HasWaiter(id CorrelationID) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.pruneStaleLocked(time.Now())

	_, ok := r.waiters[id]

	return ok
}

// FailAll completes every registered waiter's promise with err and clears the
// waiter set. It is used to fail all in-flight unary callers at once when the
// connector transitions to a terminal incompatible state, so no caller blocks
// on a response that will never arrive.
func (r *ResponseRegistry) FailAll(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	for id, waiter := range r.waiters {
		waiter.Promise.Complete(fn.Err[*mailboxpb.Envelope](err))

		delete(r.waiters, id)
	}
}

// RemovePending drops any buffered early response for the correlation ID.
func (r *ResponseRegistry) RemovePending(id CorrelationID) {
	r.mu.Lock()
	defer r.mu.Unlock()

	delete(r.pending, id)
}

// DeliverResponse delivers a response envelope for correlation ID id.
//
// If a waiter exists, the promise is completed with the envelope. If a waiter
// does not yet exist, the first response is buffered so a later RegisterWaiter
// call still receives it.
func (r *ResponseRegistry) DeliverResponse(
	id CorrelationID, env *mailboxpb.Envelope,
) DeliveryResult {

	if env == nil {
		return DeliveryDropped
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.pruneStaleLocked(time.Now())

	if waiter, ok := r.waiters[id]; ok {
		waiter.Promise.Complete(fn.Ok(env))

		return DeliveryWaiter
	}

	if _, exists := r.pending[id]; exists {
		return DeliveryBuffered
	}

	responseCopy, ok := proto.Clone(env).(*mailboxpb.Envelope)
	if !ok {
		return DeliveryDropped
	}

	r.pending[id] = &bufferedResponse{
		Envelope: responseCopy,
		Created:  time.Now(),
	}

	return DeliveryBuffered
}

// pruneStaleLocked removes stale waiters and buffered responses. Stale
// waiters have their promises completed with ErrWaiterExpired so blocked
// callers wake up with a clear error rather than hanging.
func (r *ResponseRegistry) pruneStaleLocked(now time.Time) {
	if r.waiterTTL <= 0 {
		return
	}

	for id, waiter := range r.waiters {
		if now.Sub(waiter.Created) > r.waiterTTL {
			waiter.Promise.Complete(
				fn.Err[*mailboxpb.Envelope](
					ErrWaiterExpired,
				),
			)

			delete(r.waiters, id)
		}
	}

	for id, response := range r.pending {
		if now.Sub(response.Created) > r.waiterTTL {
			delete(r.pending, id)
		}
	}
}
