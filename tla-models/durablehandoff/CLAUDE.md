# tla-models/durablehandoff

## Purpose

Exhaustively model the transaction and recovery boundary between a source
durable mailbox and a dynamically named target consumer.

## Key Files

- `DurableHandoff.tla` contains the shared transition system and properties.
- `Production.cfg` enables every required production guarantee.
- `SplitHandoff.cfg` makes source acknowledgement and target enqueue separate
  commits.
- `OmitDelayedRecovery.cfg` excludes delayed rows from restart discovery.
- `OmitLeasedRecovery.cfg` excludes leased rows from restart discovery.
- `ReapTerminalConsumer.cfg` removes a terminal consumer while durable work
  remains.

## Invariants

- Source acknowledgement implies a durable target row already exists.
- A failed fold leaves the source row pending for retry.
- Every pending target row has a consumer whenever the runtime is up,
  including delayed and leased rows after restart.
- A terminal domain state does not remove the consumer while durable target
  work remains.

## Deep Docs

- [`README.md`](README.md) explains the abstraction and checked profiles.
- [`docs/durable_actor_architecture.md`](../../docs/durable_actor_architecture.md)
  describes the concrete durable actor lifecycle.
