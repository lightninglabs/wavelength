# p-models/durableactor/bridge

## Purpose

Go conformance harness that replays JSON-encoded mailbox traces against the
real `db/actordelivery` SQLite store, verifying that the P model specification
matches the production implementation. Each trace encodes a sequence of store
operations (enqueue, lease, nack, ack, commit, dead-letter, expire-leases) and
the expected outcomes; `ReplayMailboxTrace` drives the real `TxAwareDeliveryStore`
through each step and fails the test on any deviation.

## Key Types

- `MailboxTrace` — top-level trace container: `TraceID`, `Description`, and
  `Events []MailboxTraceEvent`.
- `MailboxTraceEvent` — one store operation with fields `Op` (enqueue / lease /
  nack / ack / commit / dead_letter / expire_leases), `ID`, `MailboxID`,
  `CorrelationKey`, `LeaseToken`, `Payload`, `ExpectDuplicate`,
  `ExpectProcessed`, and related assertion flags.
- `ParseMailboxTrace(path string)` — parses a single JSON trace file.
- `ParseMailboxTraceDir(dir string)` — parses all JSON trace files in a
  directory, sorted by TraceID.
- `ReplayMailboxTrace(t *testing.T, trace *MailboxTrace)` — main entry point;
  creates an in-memory SQLite DB, constructs the real store, replays every
  event, and asserts expected outcomes.

## Relationships

- **Depends on**: `baselib/actor` (`TxAwareDeliveryStore`, `ExecTx` fence
  pattern), `db/actordelivery` (production store implementation), `db/sqlc`
  (generated query layer).
- **Depended on by**: `p-models/durableactor` (trace replay via
  `go test ./p-models/durableactor/bridge`). No production code imports this
  package.

## Invariants

- Bridge tests use real SQLite, not mocks — the point is to catch SQL/Go
  divergence from the P model, not to test the model against itself.
- `replayCommit` models the Read/Commit fence: ack inside `ExecTx` is
  atomic with the processed-mark; a lease lost mid-IO rolls back the
  entire transaction via `ErrLeaseLost`.
- JSON trace files under `../traces/` are the canonical checked-in scenarios;
  add a new trace for every P model scenario that should be replayed against Go.

## Deep Docs

- [p-models/durableactor/CLAUDE.md](../CLAUDE.md) — P model overview and
  commands.
- [docs/durable_actor_architecture.md](../../../docs/durable_actor_architecture.md)
  — Durable actor internals and the Read/Commit pattern.
- [ARCHITECTURE.md](../../../ARCHITECTURE.md) — System-wide package map.
