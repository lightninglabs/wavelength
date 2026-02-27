package rounds

import "context"

// OutboxHandler executes FSM outbox requests and returns zero or more
// follow-up inbox events to feed back into the FSM. This mirrors the OOR
// package's OutboxHandler pattern: the FSM emits pure outbox structs
// describing side effects, and the handler performs the actual I/O, returning
// result events that drive the FSM forward.
//
// A nil handler is safe — askAndDrive simply skips the handler call and
// returns the accumulated outbox for legacy processOutbox routing.
type OutboxHandler interface {
	// Handle executes the outbox request and returns follow-up events.
	Handle(ctx context.Context, roundID RoundID,
		outbox OutboxEvent) ([]Event, error)
}

// InProcessOutboxHandler is a concrete OutboxHandler that executes outbox
// requests in-process using the provided store interfaces. It is used by
// both the actor (production wiring) and tests.
//
// Outbox event types that are not yet migrated to the handler return nil
// (no follow-up events, no error), allowing the legacy processOutbox path
// to handle them.
type InProcessOutboxHandler struct {
	roundStore RoundStore
	vtxoStore  VTXOStore
}

// NewInProcessOutboxHandler creates an InProcessOutboxHandler with the given
// store dependencies.
func NewInProcessOutboxHandler(roundStore RoundStore,
	vtxoStore VTXOStore) *InProcessOutboxHandler {

	return &InProcessOutboxHandler{
		roundStore: roundStore,
		vtxoStore:  vtxoStore,
	}
}

// Handle executes the outbox request and returns follow-up events.
func (h *InProcessOutboxHandler) Handle(_ context.Context, _ RoundID,
	_ OutboxEvent) ([]Event, error) {

	// No outbox event types are handled yet. As individual FSM
	// transitions are purified, cases will be added here.
	return nil, nil
}

// Compile-time check that InProcessOutboxHandler implements OutboxHandler.
var _ OutboxHandler = (*InProcessOutboxHandler)(nil)
