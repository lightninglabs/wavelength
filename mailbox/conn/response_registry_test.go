package conn

import (
	"testing"
	"time"

	mailboxpb "github.com/lightninglabs/wavelength/mailbox/pb"
	"github.com/stretchr/testify/require"
)

// TestResponseRegistry_DeliverBeforeRegister verifies an early response is
// buffered and later delivered when a waiter registers.
func TestResponseRegistry_DeliverBeforeRegister(t *testing.T) {
	t.Parallel()

	registry := NewResponseRegistry(time.Minute)
	id := CorrelationID("corr-1")
	env := &mailboxpb.Envelope{
		MsgId: "msg-1",
	}

	delivered := registry.DeliverResponse(id, env)
	require.Equal(t, DeliveryBuffered, delivered)

	future := registry.RegisterWaiter(id)

	// The future should already be completed with the buffered response.
	got := future.Await(t.Context()).UnwrapOrFail(t)
	require.Equal(t, env.MsgId, got.MsgId)
}

// TestResponseRegistry_RegisterThenDeliver verifies an active waiter receives
// a delivered response.
func TestResponseRegistry_RegisterThenDeliver(t *testing.T) {
	t.Parallel()

	registry := NewResponseRegistry(time.Minute)
	id := CorrelationID("corr-2")
	future := registry.RegisterWaiter(id)

	delivered := registry.DeliverResponse(id, &mailboxpb.Envelope{
		MsgId: "msg-2",
	})
	require.Equal(t, DeliveryWaiter, delivered)

	got := future.Await(t.Context()).UnwrapOrFail(t)
	require.Equal(t, "msg-2", got.MsgId)
}

// TestResponseRegistry_TTLPrunesPending verifies stale pending responses are
// removed after TTL expiry.
func TestResponseRegistry_TTLPrunesPending(t *testing.T) {
	t.Parallel()

	registry := NewResponseRegistry(time.Minute)
	id := CorrelationID("corr-3")

	require.Equal(
		t, DeliveryBuffered,
		registry.DeliverResponse(
			id, &mailboxpb.Envelope{
				MsgId: "stale",
			},
		),
	)

	registry.mu.Lock()
	registry.pending[id].Created = time.Now().Add(-2 * time.Minute)
	registry.mu.Unlock()

	// Register after the buffered response has expired. The future
	// should not be immediately completed.
	future := registry.RegisterWaiter(id)

	// Deliver a fresh response to complete the future; the stale one
	// should have been pruned.
	registry.DeliverResponse(id, &mailboxpb.Envelope{
		MsgId: "fresh",
	})

	got := future.Await(t.Context()).UnwrapOrFail(t)
	require.Equal(t, "fresh", got.MsgId)
}

// TestResponseRegistry_TTLPrunesWaiter verifies that a stale waiter is pruned
// and the blocked Future receives ErrWaiterExpired.
func TestResponseRegistry_TTLPrunesWaiter(t *testing.T) {
	t.Parallel()

	registry := NewResponseRegistry(5 * time.Millisecond)
	id := CorrelationID("corr-expire")
	future := registry.RegisterWaiter(id)

	time.Sleep(20 * time.Millisecond)

	// Trigger prune by registering a different waiter.
	registry.RegisterWaiter(CorrelationID("trigger-prune"))

	result := future.Await(t.Context())
	require.ErrorIs(t, result.Err(), ErrWaiterExpired)
}

// TestResponseRegistry_RemoveWaiterSignalsCancelled verifies that removing a
// waiter completes the Future with ErrWaiterCancelled.
func TestResponseRegistry_RemoveWaiterSignalsCancelled(t *testing.T) {
	t.Parallel()

	registry := NewResponseRegistry(time.Minute)
	id := CorrelationID("corr-cancel")
	future := registry.RegisterWaiter(id)

	registry.RemoveWaiter(id)

	result := future.Await(t.Context())
	require.ErrorIs(t, result.Err(), ErrWaiterCancelled)
}

// TestResponseRegistry_DeliverNilReturnsFalse verifies nil envelope
// delivery is rejected.
func TestResponseRegistry_DeliverNilReturnsFalse(t *testing.T) {
	t.Parallel()

	registry := NewResponseRegistry(time.Minute)
	require.Equal(t, DeliveryDropped, registry.DeliverResponse("any",
		nil))
}

// TestResponseRegistryHasWaiterTracksRegistration verifies HasWaiter reflects
// the live-waiter state the ingress split path relies on: false before
// registration, true while a waiter is registered, and false again once the
// waiter is removed or a buffered (waiterless) response is delivered.
func TestResponseRegistryHasWaiterTracksRegistration(t *testing.T) {
	t.Parallel()

	registry := NewResponseRegistry(time.Minute)
	id := CorrelationID("corr-has-waiter")

	// No waiter registered yet.
	require.False(t, registry.HasWaiter(id))

	// A buffered early response does not count as a live waiter, so the
	// ingress loop folds it into the durable transaction.
	registry.DeliverResponse(id, &mailboxpb.Envelope{EventSeq: 1})
	require.False(t, registry.HasWaiter(id))

	// Registering a waiter flips it true; this drains the buffered
	// response into the promise.
	registry.RegisterWaiter(id)
	require.True(t, registry.HasWaiter(id))

	// Removing the waiter flips it back to false.
	registry.RemoveWaiter(id)
	require.False(t, registry.HasWaiter(id))
}

// TestResponseRegistryHasWaiterPrunesStale verifies HasWaiter prunes an expired
// waiter before answering, so a TTL-lapsed entry never masquerades as live and
// misroutes a response onto the fast path.
func TestResponseRegistryHasWaiterPrunesStale(t *testing.T) {
	t.Parallel()

	registry := NewResponseRegistry(5 * time.Millisecond)
	id := CorrelationID("corr-stale")

	registry.RegisterWaiter(id)
	require.True(t, registry.HasWaiter(id))

	time.Sleep(10 * time.Millisecond)

	require.False(t, registry.HasWaiter(id))
}
