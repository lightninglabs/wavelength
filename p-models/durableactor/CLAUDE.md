# p-models/durableactor

## Purpose

Executable P model of the durable actor mailbox: durable enqueue, lease
ownership, retry/backoff, ack/nack token validation, dead-letter, per-
correlation-key FIFO, the Read/Commit exactly-once consume step, the Stage
persist-before-broadcast primitive, and the CDC outbox/ingress folds. Treated
as the ideal specification the Go implementation must conform to.

## Key Types

- `DurableMailboxSpec` (`src/mailbox_fifo.p`) — stateful machine modeling the
  mailbox: enqueue, lease/peek claim, ack/nack (by token or by id), dead
  letter, and the fenced `eDurableMailboxCommit` / `eDurableMailboxStage`
  consume steps.
- `OutboxFoldSpec` (`src/mailbox_fifo.p`) — models the CDC outbox: target
  enqueue and outbox completion as one atomic fold.
- `IngressCursorCoversOnlyCommittedEnvelopes` (`src/ingress_fold.p`) — spec
  monitor guarding the connection-actor ingress cursor: the persisted cursor
  must never cover an envelope whose local enqueue did not commit.
- Safety/liveness monitors (`src/mailbox_fifo.p`): `SameKeyFIFOClaimsRespectLiveHead`,
  `MailboxKeyedWorkEventuallyDrains`, `LeaseFencedCommitAppliesEffectAtMostOnce`,
  `StagedEffectAppliedAtMostOnceUnderReplay`, `CheckpointAdvancesMonotonically`.

## Relationships

- **Depends on**: nothing in-repo (self-contained P sources); conceptually
  models `baselib/actor` (Read/Commit, Stage) and `db/actordelivery` (claim
  SQL, cursor persistence).
- **Depended on by**: `p-models/durableactor/bridge` (replays `traces/*.json`
  scenarios from this model against the real `db/actordelivery` store).

## Invariants

- Keep the default P test case green; put intentional known-bad checks in a
  separate "Counterexample" test case (`test/mailbox_fifo_test.p`,
  `test/ingress_fold_test.p`).
- `0` is the model's NULL correlation key; non-zero keys are per-lane FIFO
  domains scoped by mailbox id.
- Every safety/liveness monitor above is opt-in per test case (`assert <spec>
  in { ... }`) — P does not activate them globally.
- New correctness properties should be expressed both as a direct P scenario
  and as a `traces/*.json` bridge trace.

## Deep Docs

- [README.md](README.md) — Full model walkthrough, monitor semantics, trace
  authoring notes, and `p check` / `go test` commands.
- [p-models/CLAUDE.md](../CLAUDE.md) — Top-level p-models layout and shared
  commands.
- [docs/durable_actor_architecture.md](../../docs/durable_actor_architecture.md)
  — Durable actor internals the model conforms to.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
