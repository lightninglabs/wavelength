# Recover wallet work across host suspension

This is a living ExecPlan maintained under `PLANS.md`. The Progress, Surprises & Discoveries, Decision Log, and Outcomes & Retrospective sections must stay current.

## Purpose / Big Picture

A mobile wallet can be terminated after acknowledging a server message. The work admitted by that acknowledgement must survive in a persistent local inbox, and its state and outgoing effects must commit together. This first implementation supplies the runtime/recovery boundary behind the future mobile wake API. Push delivery, notification servers, language wrappers, and app screens belong to separate PRs. No deployment or production writes are authorized.

## Progress

- [x] (2026-11-06) Fetch current main and inspect #1329, #1504, #1517, and the earlier #1334 consumer prototype.
- [x] Create isolated worktree `wt-wavelength-mobile-runtime-recovery-20261106` and branch `mobile-runtime-recovery-20261106` from main `b8658164`.
- [x] Isolate #1517's two commits and #1329 as dependency branch `stack/mobile-runtime-prerequisites-20261106`; #1504 is already merged. Dependency tip is `fbac0db6`.
- [x] Extract additive consumer helpers from #1334 without replacing the current #1517 checkpoint codec or importing its unrelated batch-service protocol.
- [x] Compile the round package with the extracted runtime and preserve the threaded runner for ordinary actors.
- [x] Complete current admission/scheduling/accounting/boarding-queue contracts and integrate the opt-in runtime.
- [x] Prove SQL rollback of attempted effects, retained Tell input, and restart convergence on SQLite and PostgreSQL. Test signing interruption classification, no cold signing replay, timer replacement, absolute scheduling, boarding correlation and deferred ledger capture.
- [x] Keep full bounded wallet execution and safe-to-suspend reporting in the later host API PR; this PR supplies the persistent consumer and host-driven ingress startup seam.
- [x] Run formatting, lint (including actorblock), module tidiness, package suites, focused race checks and both real SQL backends.
- [x] Publish signed implementation PR #1531 with a diff that excludes the copied prerequisites. The published base is `mobile-runtime-prerequisites-20261106`; the original local `stack/` branch remains an alias.
- [x] Complete initial Gateway review and current-head checks.
- [x] Refresh prerequisites on current main and retain composed boarding authorization paths.
- [ ] Verify refreshed-head Gateway review and CI.

## Surprises & Discoveries

The prototype predates durable admission deadlines, nonblocking boarding queries, ledger finalization callbacks, and later policy metadata. The extracted consumer now includes those contracts and real-store failure tests. In particular an empty remote mailbox cannot prove local completion, and private MuSig2 sessions cannot be recreated from a saved public checkpoint.

## Decision Log

Use the merged synchronous runner only for the persistent round owner. The default daemon keeps its original independently driven state machines. Reuse #1517's encoding rather than introducing a parallel protocol-state format. Keep the predecessor commits on a named dependency branch so reviewers see only this implementation; retarget to main after those predecessors merge. Do not merge dependencies or self-approve reviews merely to satisfy readiness.

External signing executes outside the database writer. Before entering a signing call, persist sufficient attempt ownership to detect interruption. Commit domain state, planned effects, the actor checkpoint, and consumed input through the existing Read/Stage/Commit durable actor API. On commit failure reload durable state. Cold recovery must never create fresh private signing sessions for an old ceremony. Signature-bearing checkpoints retain exact signatures and reconcile with the operator; pre-checkpoint recovery preserves intent and does not infer that a timeout proves an old side effect absent.

## Outcomes & Retrospective

The opt-in durable consumer, full owner snapshot, signing fence, and atomic final commit are implemented. SQLite and PostgreSQL restart/rollback tests pass, as do the round, waved, serverconn and actordelivery package suites. Focused race checks pass. Formatting, lint including actorblock, module tidiness and new-file documentation audit pass. Published as #1531. Gateway found retained timers and an expected cold failure push entering retry. Both are fixed with regression coverage; current-head re-review and CI are pending. Read-only round-query snapshot cost is a performance follow-up. No deployment has occurred.

## Context and Orientation

`serverconn/ingress_pump.go` from #1329 provides bounded inbound pulls and internal cursor persistence. `round/actor.go` uses its ordinary in-memory runner by default; durable mode receives protocol messages through the native persistent mailbox and runs inline. `round/durable_state.go` from #1517 serializes all protocol states and reports lost private signer sessions. `baselib/protofsm/inline_machine.go` from #1504 runs an entire internal-event chain in the owner's turn. `baselib/actor/tx_exec.go` supplies separate read, persist-before-side-effect stage, and atomic final commit transactions. `db/actordelivery` is the shared SQLite/PostgreSQL mailbox/outbox store. Domain stores must join its context transaction. `waved/server.go` wires actor references, event routes, and the shared outbox publisher. `sdk/wavewalletdk` owns embedded startup; higher-level mobile APIs remain a later PR.

A checkpoint is persisted recovery data. An inbox is a database queue of accepted work. An outbox is a database queue of effects to deliver after their state transition commits. MuSig2 is the interactive joint signing protocol whose private process-local session cannot be reconstructed by replaying an old event.

## Plan of Work

Adapt the extracted `round/durable_*` consumer envelopes, owner snapshot, effects, and runtime to current fields. Preserve original timer deadlines, round-bound reservation releases, boarding-query ownership, admission expiry, and ledger metadata. Add precise failure tests before enabling any runtime path. Integrate the durable owner through an opt-in programmatic configuration and register its effect codec with the shared publisher. Keep ordinary foreground startup compatible. Expose only the programmatic paused-ingress startup seam in this PR. The later host API must serialize execution windows, gate unsupported work before dispatch, join local work, and report completion honestly.

## Concrete Steps

Work only in this worktree. Use `go test ./round -run '^$'` during adaptation. Then use `make unit pkg=round` and real file-backed SQLite/PostgreSQL tests for persistence boundaries, followed by targeted race runs. Run `make fmt-changed`, `make lint-changed-local`, Go declaration audit, module tidiness, and commit-message checks before signed commits. Use the actual Makefile flags and database harnesses. Keep dependency validation distinct from tests of the new implementation.

## Validation and Acceptance

A committed remote acknowledgement must correspond to either recoverable local work or an explicitly handled rejection. Terminate after ingress dispatch, before/after the actor commit, between signed-state persistence and effect delivery, and around acknowledgement persistence. Reopen the same temporary database and prove unchanged operation identity, no duplicate funding/signing, replay of the exact committed effects, and convergence. Kill during signer creation, including the first turn, and prove no duplicate ceremony starts. Scheduled waits and admitted deadlines must not renew after restart. Confirmed accounting must retain all current metadata. Background cancellation must not close storage while workers still use it. Credits and micropayments must remain available. Local tests cannot establish iOS runtime grants, APNs delivery, or power-loss durability.

## Idempotence and Recovery

Use disposable test databases only. Never reset an existing wallet database. An unsuccessful transaction must leave the prior checkpoint and unconsumed input available for retry. Replayed effects retain their committed identifiers. Any integration that cannot prove safe handoff remains disabled. Gateway findings require a concrete reachable trace and the smallest useful correction; stop expanding scope once the invariant and required checks are proven.

## Artifacts and Notes

Current package suites and focused race checks pass. The SQL test injects failure after a cancellation turn attempts its outbox writes and before the final checkpoint commits. It proves the effect rolls back, the accepted Tell remains queued, and restart commits one reservation release. Synchronous Ask errors retain the native caller-visible failure contract; they are not treated as remote Tell retry. Combined real-store confirmation plus nonempty ledger rollback remains a useful follow-up beyond the covered deferred accounting invariant. Current dependency heads: #1329 `abf6e05b`, #1517 `9430ebe6`, merged #1504 `0187072d`.

## Interfaces and Dependencies

Keep `Runtime.PumpIngress` and `PauseIngress` as the transport primitives. Use `round.NewDurableRoundClientActor`, typed `Ref`, outbox target registration, and a separately owned stop/join lifecycle for persistent round work. Public SDK/mobile method names belong to the subsequent API PR. Use native TLV envelopes for durable payloads; never serialize actor references or Go contexts.

Revision: records the completed runtime adaptation, tested recovery invariants, explicit host API boundary, and remaining publication/review work.

## Current-main compatibility

The asset-boarding merge adds BoardingIntent.AuthSpend. Full-field checkpoint
fixtures detected that the older prototype omitted it. The runtime now stores
its existing stable arkscript encoding, including witness script, control block,
conditions and timelocks. A real composed asset boarding path still binds to the
same funding script after restore.

New client-state snapshots write version 2. Old readers reject that version
instead of silently discarding authorization metadata. New readers still accept
version 1 Bitcoin snapshots. Old asset snapshots missing the authorization
path and unknown versions fail closed rather than substituting another leaf. The refreshed base
is mobile-runtime-prerequisites-v2-20261107 at 8c85fdfc, combining current main
with the same isolated ingress/checkpoint prerequisites. No deployment or
persistent database migration is involved.
