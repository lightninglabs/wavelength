# p-models/durableactor/bridge

## Purpose

Go conformance harness that replays P model mailbox traces against the real
`db/actordelivery` SQLite store. Keeps the formal P model abstraction tied to
the SQL claim implementation: every P scenario in `mailbox_fifo_test.p`
that produces a trace is replayed here using production store code, so a
divergence between the model and the implementation fails the Go test rather
than the P checker.

## Key Types

- `MailboxTrace` — A named sequence of mailbox operations loaded from a JSON
  trace file (`trace_id`, `description`, `events`).
- `MailboxTraceEvent` — One store operation in a trace: `op` (enqueue/lease/
  nack/ack/commit/dead_letter/expire_leases), plus op-specific fields for id,
  mailbox_id, lease_token, expected outcome, etc. `ExpectDuplicate` asserts
  idempotent no-op enqueue semantics. `ExpectProcessed` verifies the dedup
  mark after a fenced commit.
- `ParseMailboxTrace(path)` — Parses one trace file from disk.
- `ParseMailboxTraceDir(dir)` — Parses all `*.json` trace files in a directory,
  sorted by `TraceID`.
- `ReplayMailboxTrace(t, trace)` — Replays a trace against a fresh SQLite
  `actordelivery` store in a temp dir. The `commit` op models the Read/Commit
  fenced-ack pattern exactly: it runs `AckMessage` + `MarkProcessed` inside one
  writer transaction, rolling back with `actor.ErrLeaseLost` when the ack row
  count is zero.

## Relationships

- **Depends on**: `db/actordelivery` (real store under test), `baselib/actor`
  (store interfaces, `ErrLeaseLost`), `db/sqlc` (backend type constants).
- **Depended on by**: nothing (test-only package, invoked via
  `go test ./p-models/durableactor/bridge`).

## Invariants

- Every trace op that can fail hard uses `t.Fatal`; partial replays are not
  allowed to proceed silently.
- The `commit` op is the sole site where `ExecTx`/`AckMessage`/`MarkProcessed`
  are combined — it deliberately mirrors `execCore.commit` in `baselib/actor`
  so the P model's commit-fence scenario stays tied to the real SQL path.
- Duplicate enqueue ops (`ExpectDuplicate: true`) must complete without error;
  a future rejection would fail here explicitly rather than at a later lease
  step.

## Deep Docs

- [p-models/durableactor/CLAUDE.md](../CLAUDE.md) — P model structure, trace
  layout, and check commands.
- [p-models/CLAUDE.md](../../CLAUDE.md) — Top-level p-models layout and
  orchestration.
- [docs/durable_actor_architecture.md](../../../docs/durable_actor_architecture.md)
  — Durable actor internals.
