// Package actortest provides end-to-end integration tests for the durable actor
// system. These tests exercise the full stack using real database backends
// (SQLite and Postgres) rather than mocks, ensuring production-like behavior.
//
// The package includes a demo CounterActor that demonstrates all durable actor
// features:
//   - TLVMessage serialization for all message types
//   - Tell and Ask patterns
//   - FSM state checkpointing
//   - Outbox for inter-actor communication
//   - Crash recovery with message redelivery
//   - Deduplication across restarts
//
// These tests verify the distributed systems invariants defined in the plan:
//   - At-least-once delivery
//   - Exactly-once processing via deduplication
//   - FIFO ordering within priority class
//   - Atomic state + outbox writes
//   - Bounded retries with dead-lettering
package actortest
