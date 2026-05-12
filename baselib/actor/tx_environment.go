package actor

// TxEnvironment is an interface that environments can implement to support
// transaction-scoped operations. This enables FSM states to access a querier
// that participates in the same database transaction as the actor's message
// processing.
//
// The pattern works as follows:
//  1. DurableActor begins a database transaction
//  2. It calls env.WithQuerier(q) to create a tx-scoped environment
//  3. The FSM's ProcessEvent receives this tx-scoped environment
//  4. FSM states can access the querier via env.Querier()
//  5. All persistence operations participate in the same transaction
//
// Example implementation:
//
//	type DurableRoundEnvironment struct {
//	    *ClientEnvironment
//	    querier DurableRoundQuerier
//	}
//
//	func (e *DurableRoundEnvironment) WithQuerier(q DurableRoundQuerier) *DurableRoundEnvironment {
//	    return &DurableRoundEnvironment{
//	        ClientEnvironment: e.ClientEnvironment,
//	        querier:           q,
//	    }
//	}
//
//	func (e *DurableRoundEnvironment) Querier() DurableRoundQuerier {
//	    return e.querier
//	}
type TxEnvironment[Q any] interface {
	// WithQuerier returns a new environment instance that uses the provided
	// querier for all database operations. The returned environment should
	// be used only for the lifetime of the transaction.
	WithQuerier(q Q) TxEnvironment[Q]

	// Querier returns the current querier, or nil if not in a transaction.
	// FSM states should check for nil before using the querier.
	Querier() Q
}

// OutboxWriter defines the interface for writing messages to the transactional
// outbox. This is used by FSM states to enqueue messages to other actors
// within the same transaction as state changes.
type OutboxWriter interface {
	// WriteToOutbox enqueues a message to the transactional outbox. The
	// message will be delivered to the target actor by the OutboxPublisher
	// after the transaction commits.
	WriteToOutbox(params OutboxParams) error
}
