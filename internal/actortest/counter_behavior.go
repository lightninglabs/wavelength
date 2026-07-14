package actortest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/google/uuid"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightningnetwork/lnd/fn/v2"
)

// ErrUnhandledMessage indicates the actor received a message type it doesn't
// know how to process.
var ErrUnhandledMessage = errors.New("unhandled message type")

// CounterBehavior implements ActorBehavior for a simple counter actor.
// It supports increment, decrement, get, and forward operations.
//
// The counter demonstrates:
//   - Stateful actor with in-memory state (count)
//   - Tell pattern for fire-and-forget updates
//   - Ask pattern for request/response queries
//   - Outbox pattern for message forwarding (CDC)
//   - DurableAsk pattern for crash-safe request/response
type CounterBehavior struct {
	// actorID is the unique identifier for this actor.
	actorID string

	// count is the current counter value. Uses atomic for safe concurrent
	// reads (e.g., for testing assertions) while writes happen serially
	// via the actor's message processing loop.
	count atomic.Int64

	// store is the delivery store for outbox operations.
	store actor.DeliveryStore

	// codec is the message codec for serializing forwarded messages.
	codec *actor.MessageCodec

	// forwardCount tracks how many messages were forwarded via outbox.
	forwardCount atomic.Int64

	// askResponses stores received AskResponse messages for testing.
	askResponses   []*actor.AskResponse
	askResponsesMu sync.Mutex

	// forceError, if set, causes all requests to fail with this error.
	forceError   error
	forceErrorMu sync.RWMutex
}

// NewCounterBehavior creates a new counter behavior.
func NewCounterBehavior(
	actorID string,
	store actor.DeliveryStore,
	codec *actor.MessageCodec,
) *CounterBehavior {

	return &CounterBehavior{
		actorID: actorID,
		store:   store,
		codec:   codec,
	}
}

// Receive processes incoming messages and returns a result.
func (b *CounterBehavior) Receive(
	ctx context.Context,
	msg CounterMessage,
) fn.Result[CounterResult] {

	// Check if we should force an error for testing.
	b.forceErrorMu.RLock()
	forceErr := b.forceError
	b.forceErrorMu.RUnlock()

	if forceErr != nil {
		return fn.Err[CounterResult](forceErr)
	}

	switch m := msg.(type) {
	case *IncrementMsg:
		newVal := b.count.Add(m.Amount)

		return fn.Ok(newVal)

	case *DecrementMsg:
		newVal := b.count.Add(-m.Amount)

		return fn.Ok(newVal)

	case *GetCountMsg:
		return fn.Ok(b.count.Load())

	case *ForwardMsg:
		// Write to outbox for async delivery to target actor.
		// This exercises the CDC pattern where the message is committed
		// atomically with any FSM state changes, then picked up by the
		// OutboxPublisher for delivery.
		err := b.writeToOutbox(ctx, m)
		if err != nil {
			return fn.Err[CounterResult](err)
		}

		b.forwardCount.Add(1)

		return fn.Ok(b.forwardCount.Load())

	case *actor.AskResponse:
		// Store the AskResponse for testing verification.
		b.askResponsesMu.Lock()
		b.askResponses = append(b.askResponses, m)
		b.askResponsesMu.Unlock()

		return fn.Ok(CounterResult(0))

	default:
		return fn.Err[CounterResult](ErrUnhandledMessage)
	}
}

// writeToOutbox writes a forwarded message to the outbox table.
func (b *CounterBehavior) writeToOutbox(
	ctx context.Context,
	m *ForwardMsg,
) error {

	// Generate UUID v7 for message ID.
	id, err := uuid.NewV7()
	if err != nil {
		return fmt.Errorf("generate uuid: %w", err)
	}

	params := actor.OutboxParams{
		ID:            id.String(),
		SourceActorID: b.actorID,
		TargetActorID: m.Target,
		MessageType:   fmt.Sprintf("tlv.Type(%d)", m.MsgType),
		Payload:       m.Payload,
		Version:       b.forwardCount.Load() + 1,
	}

	if err := b.store.EnqueueOutbox(ctx, params); err != nil {
		return fmt.Errorf("enqueue outbox: %w", err)
	}

	return nil
}

// Count returns the current counter value. Safe for concurrent access.
func (b *CounterBehavior) Count() int64 {
	return b.count.Load()
}

// SetCount sets the counter value. Used for testing/recovery scenarios.
func (b *CounterBehavior) SetCount(val int64) {
	b.count.Store(val)
}

// ForwardCount returns the number of messages forwarded via outbox.
func (b *CounterBehavior) ForwardCount() int64 {
	return b.forwardCount.Load()
}

// SetForceError sets an error that will be returned for all requests.
// Pass nil to clear the forced error.
func (b *CounterBehavior) SetForceError(err error) {
	b.forceErrorMu.Lock()
	b.forceError = err
	b.forceErrorMu.Unlock()
}

// LastAskResponse returns the most recently received AskResponse, or nil.
func (b *CounterBehavior) LastAskResponse() *actor.AskResponse {
	b.askResponsesMu.Lock()
	defer b.askResponsesMu.Unlock()

	if len(b.askResponses) == 0 {
		return nil
	}

	return b.askResponses[len(b.askResponses)-1]
}

// AskResponseCount returns the number of AskResponses received.
func (b *CounterBehavior) AskResponseCount() int {
	b.askResponsesMu.Lock()
	defer b.askResponsesMu.Unlock()

	return len(b.askResponses)
}

// ReceivedCorrelationIDs returns all correlation IDs from received responses.
func (b *CounterBehavior) ReceivedCorrelationIDs() []string {
	b.askResponsesMu.Lock()
	defer b.askResponsesMu.Unlock()

	ids := make([]string, len(b.askResponses))
	for i, r := range b.askResponses {
		ids[i] = r.CorrelationID
	}

	return ids
}

// Compile-time interface check.
var _ actor.ActorBehavior[CounterMessage, CounterResult] = (*CounterBehavior)(
	nil,
)
