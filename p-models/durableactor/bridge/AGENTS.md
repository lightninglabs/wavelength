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
  peek/nack/nack_by_id/ack/ack_by_id/commit/dead_letter/expire_leases), plus
  op-specific fields for id, mailbox_id, lease_token, expected outcome, etc.
  `peek` replays `PeekNextMessage` (the leaseless single-worker claim) and
  asserts the returned row's id/payload/attempts/empty lease token;
  `nack_by_id`/`ack_by_id` replay the unfenced by-ID store ops the leaseless
  consume path uses. `ExpectAttempts` / `ExpectToken` assert the leased or
  peeked row's attempt count and lease token. `ExpectDuplicate` asserts
  idempotent no-op enqueue semantics. `ExpectProcessed` verifies the dedup
  mark after a fenced commit.
- `ParseMailboxTrace(path)` — Parses one trace file from disk.
- `ParseMailboxTraceDir(dir)` — Parses all `*.json` trace files in a directory,
  sorted by `TraceID`.
- `ReplayMailboxTrace(t, trace)` — Replays a trace against a fresh SQLite
  `actordelivery` store in a temp dir. The `commit` op models the Read/Commit
  consume step exactly: it runs the ack + `MarkProcessed` inside one writer
  transaction, rolling back with `actor.ErrLeaseLost` when the ack row count is
  zero. The ack op is chosen by lease token exactly as production `ackMessage`
  routes it: a leased commit (`lease_token` set) acks under the lease fence via
  `AckMessage`, while a leaseless commit (empty `lease_token`, the single-worker
  peek path) acks via `AckMessageByID`. Both halves are trace-covered
  (`mailbox_read_commit_fenced_exactly_once` and
  `mailbox_leaseless_commit_fold`).

## Direct bridge tests (not JSON-trace driven)

Some contracts span a transaction boundary or a process restart and do not fit
the per-op JSON replay model, so they are written as direct Go tests that drive
the real store:

- `outbox_fold_test.go` — the Go analog of the `tcOutboxFold` P scenario. It
  runs `deliverMessage`'s fold (`EnqueueMessage` + `CompleteOutbox` inside one
  `ExecTx`) against the real store and asserts the SQL-level contract: a fold
  that fails after the enqueue rolls back with no orphan in the target mailbox
  and the outbox row stays pending until claim expiry; a redelivery lands
  exactly once; and a stale-token completion is a fenced 0-row no-op while the
  idempotent (`ON CONFLICT id`) enqueue collapses a concurrent reclaim's
  duplicate.
- `crash_restart_test.go` — reopens a fresh `*sql.DB` and store against the same
  on-disk SQLite file to model a process restart. It asserts a peeked-but-unacked
  message survives a crash with attempts unchanged (peek is read-only), and a
  leased-but-unacked message survives with its attempt bump durable and becomes
  re-leasable after `ExpireLeases`.

## Relationships

- **Depends on**: `db/actordelivery` (real store under test), `baselib/actor`
  (store interfaces, `ErrLeaseLost`), `db/sqlc` (backend type constants).
- **Depended on by**: nothing (test-only package, invoked via
  `go test ./p-models/durableactor/bridge`).

## Invariants

- Every trace op that can fail hard uses `t.Fatal`; partial replays are not
  allowed to proceed silently.
- The `commit` op is the sole site where `ExecTx`/ack/`MarkProcessed` are
  combined — it deliberately mirrors `execCore.commit` in `baselib/actor`,
  routing the ack to `AckMessage` (leased) or `AckMessageByID` (leaseless,
  empty token) exactly as production `ackMessage` does, so the P model's
  commit-fence scenario stays tied to the real SQL path on both the leased and
  leaseless single-worker consume.
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
