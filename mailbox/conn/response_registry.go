package conn

import (
	"fmt"
	"sync"
	"time"

	"github.com/lightninglabs/darepo-client/baselib/actor"
	mailboxpb "github.com/lightninglabs/darepo-client/mailbox/pb"
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

// DeliverResponse delivers a response envelope for correlation ID id.
//
// If a waiter exists, the promise is completed with the envelope. If a waiter
// does not yet exist, the first response is buffered so a later RegisterWaiter
// call still receives it.
func (r *ResponseRegistry) DeliverResponse(
	id CorrelationID, env *mailboxpb.Envelope,
) bool {

	if env == nil {
		return false
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.pruneStaleLocked(time.Now())

	if waiter, ok := r.waiters[id]; ok {
		waiter.Promise.Complete(fn.Ok(env))

		return true
	}

	if _, exists := r.pending[id]; exists {
		return true
	}

	responseCopy, ok := proto.Clone(env).(*mailboxpb.Envelope)
	if !ok {
		return false
	}

	r.pending[id] = &bufferedResponse{
		Envelope: responseCopy,
		Created:  time.Now(),
	}

	return true
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
