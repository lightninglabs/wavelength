package rounds

// OutboxEvent is a sealed interface for all outbox messages emitted
// by the round FSM. The sealed interface pattern prevents external
// packages from implementing this interface, ensuring type safety
// and exhaustive pattern matching in state transitions.
type OutboxEvent interface {
	// outboxEventSealed is an unexported method that marks this interface
	// as sealed, preventing external implementations.
	outboxEventSealed()
}
