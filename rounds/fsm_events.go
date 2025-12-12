package rounds

// Event is a sealed interface for all events that can be processed by the
// round state machine. The sealed interface pattern prevents external packages
// from implementing this interface, ensuring type safety and exhaustive pattern
// matching in state transitions.
type Event interface {
	// eventSealed is an unexported method that marks this interface as
	// sealed, preventing external implementations.
	eventSealed()
}
