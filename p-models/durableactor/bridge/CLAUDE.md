# p-models/durableactor/bridge

## Purpose

Go conformance harness that replays P-model mailbox scenarios against the real
`db/actordelivery` SQLite delivery store. Bridges the formal P-model
specification to the concrete SQL claim implementation so model-checked traces
are automatically re-verified against production code.

## Key Types

- `MailboxTrace` — JSON model trace with `TraceID`, `Description`, and
  `Events`.
- `MailboxTraceEvent` — Single store operation. `Op` is one of:
  `enqueue`, `lease`, `nack`, `ack`, `commit`, `dead_letter`,
  `expire_leases`. `Now` advances the test clock.
- `ParseMailboxTrace(path)` — Parses one JSON trace file from disk.
- `ParseMailboxTraceDir(dir)` — Parses all trace files in a directory,
  sorted by `TraceID`.
- `ReplayMailboxTrace(t, trace)` — Replays the trace operations against a
  fresh in-memory SQLite delivery store, asserting invariants at each step.

## Relationships

- **Depends on**: `baselib/actor` (`TxAwareDeliveryStore`, `EnqueueParams`,
  `ErrLeaseLost`), `db/actordelivery` (`RunMigrations`,
  `NewTxAwareDeliveryStoreFromDB`), `db/sqlc` (`BackendTypeSqlite`).
- **Depended on by**: nothing at runtime; invoked only by `go test`.
- **Sends/Receives**: none — pure test infrastructure with no actor message
  flows.

## Invariants

- Duplicate `enqueue` operations are idempotent no-ops (assert re-enqueue
  returns without error).
- **Fenced commit**: a `commit` inside one writer transaction acks the message
  and marks it processed atomically; a stale-lease `commit` rolls back both
  the ack and the dedup mark via `ErrLeaseLost`.
- Dedup marks are written atomically with the ack inside the same writer
  transaction — no separate ack-then-dedup window.
- Trace events drive the test clock via `MailboxTraceEvent.Now`; the store is
  created with a `lnd.TestClock` so expiry and lease semantics are
  deterministic without real-time sleeps.
- `ReplayMailboxTrace` uses a fresh SQLite store per trace, so traces are
  fully isolated.

## Deep Docs

- [p-models/durableactor/CLAUDE.md](../CLAUDE.md) — Parent model overview,
  commands, and modeling guidance.
- [docs/durable_actor_architecture.md](../../../docs/durable_actor_architecture.md)
  — Durable actor internals the traces exercise.
- [ARCHITECTURE.md](../../../ARCHITECTURE.md) — System-wide package map.
