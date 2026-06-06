# p-models/durableactor/bridge

## Purpose

Go conformance harness that replays formal P model traces against the real
`db/actordelivery` SQLite durable-actor delivery store. Ties the formal
`mailbox_fifo.p` spec to the SQL claim implementation so correctness proofs
from the P model directly validate production code.

## Key Types

- `MailboxTrace` — JSON trace structure. Fields: `TraceID` (stable
  scenario identifier), `Description` (human summary), `Events`
  (`[]MailboxTraceEvent` ordered replay script).
- `MailboxTraceEvent` — One store operation in the replay. `Op` selects
  the operation (`enqueue`, `lease`, `nack`, `ack`, `commit`,
  `dead_letter`, `expire_leases`). Key assertion fields:
  `ExpectID` (expected leased row), `ExpectDuplicate` (enqueue of
  an already-present id must be a silent no-op), `ExpectProcessed`
  (tri-state dedup mark after a `commit` op — nil = unchecked, true/false
  = checked). `Now *int64` overrides the test clock (seconds relative to
  trace epoch `1_700_000_000`).
- `ParseMailboxTrace(path)` — Loads one trace from a JSON file.
- `ParseMailboxTraceDir(dir)` — Loads all `*.json` traces from a
  directory, sorted by `TraceID`.
- `ReplayMailboxTrace(t, trace)` — Drives a `TxAwareDeliveryStore` through
  each event in order, asserting preconditions and postconditions. Creates a
  fresh SQLite DB per call.
- `replayCommit` — Models the Read/Commit fenced consume: runs
  `AckMessage` + `MarkProcessed` inside one `ExecTx`. A zero-row ack
  returns `ErrLeaseLost` and rolls both writes back — exactly once per
  committed lease token.

## Relationships

- **Depends on**: `db/actordelivery` (SQLite delivery store + migrations),
  `baselib/actor` (`TxAwareDeliveryStore`, `EnqueueParams`, `ErrLeaseLost`),
  `db/sqlc` (backend type constant).
- **Depended on by**: CI (`go test ./p-models/durableactor/bridge`),
  `p-models/scripts/check.sh` (full P + Go bridge check).

## Invariants

- Each `ReplayMailboxTrace` call creates a fresh in-process SQLite DB via
  `t.TempDir()` so traces are fully isolated.
- `commitActorID = "model.TraceActor"` and `commitDedupTTL = time.Hour`
  are package-scoped constants so all commit-style trace ops share the
  same actor identity and TTL as the P model's commit step.
- Trace files live in `../traces/*.json`; new model scenarios must include
  a corresponding bridge trace (see parent `CLAUDE.md` for rules).

## Deep Docs

- [p-models/durableactor/CLAUDE.md](../CLAUDE.md) — Model overview, all P
  check commands, modeling guidance.
- [docs/durable_actor_architecture.md](../../../docs/durable_actor_architecture.md)
  — CDC pattern and durable mailbox lifecycle.
- [ARCHITECTURE.md](../../../ARCHITECTURE.md) — System-wide package map.
