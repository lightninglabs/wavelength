# vhtlcrecovery/coordinator

## Purpose

Runtime coordinator that bridges durable vHTLC recovery rows to the
generic `unroll` subsystem. Converts an armed SQL recovery row into
unroll admission via an `(exit_policy_kind, recovery_id)` tuple. Exists
as a child package to avoid an import cycle: `db` imports the parent
`vhtlcrecovery`, and `coordinator` must import both `db`-facing
interfaces and `unroll`.

## Key Types

- `Service` — Core coordinator. Intentionally thin (not a durable
  actor); all durable state is owned by the `Store`. Constructed via
  `NewService(ServiceConfig)`.
  - `ArmRecovery` — Persist a dormant recovery (idempotent).
  - `EscalateRecovery` — Write durable state to `StateUnrollStarted`
    then admit to `UnrollRegistry` (write-before-admit for crash
    safety).
  - `CancelRecovery` — Mark non-terminal row as cancelled (idempotent
    for already-terminal rows).
  - `GetRecoveryStatus` — Load row and reconcile terminal unroll
    outcomes into the durable row.
  - `ListRecoveryStatuses` — All rows with best-effort unroll status.
  - `RestoreNonTerminal` — Re-issue unroll admission for
    already-escalated rows on daemon startup; armed jobs are left
    dormant.
- `Store` — Durable persistence interface. Implementations must use SQL
  transactions for all mutations.
  - `ArmRecovery`, `GetRecovery`, `ListNonTerminalRecoveries`,
    `ListRecoveries`, `EscalateRecovery`, `CancelRecovery`,
    `CompleteRecovery`, `FailRecovery`.
- `UnrollRegistry` — Narrow unroll control surface.
  - `EnsureUnroll(ctx, req) (*EnsureUnrollResp, error)` — admit or
    deduplicate a target.
  - `GetStatus(ctx, target) (*GetStatusResp, error)` — current registry
    view.
- `ActorUnrollRegistry` — Adapts a live `baselib/actor.ActorRef` to the
  `UnrollRegistry` interface. Constructed via `NewActorUnrollRegistry`.
- `TargetMaterializer` — Domain adapter called before generic unroll
  admission. `EnsureRecoveryTarget(ctx, job)` materializes the local
  VTXO/package state the unroll registry needs (idempotent; runs on
  every escalation and restart retry).
- `ServiceConfig` — Wiring: `Store` (required), `Unroll` (required),
  `Log` (optional), `TargetMaterializer` (optional).
- `RecoveryStatus` — Joined view: durable `RecoveryJob` plus
  best-effort unroll runtime fields (`UnrollFound`, `UnrollActive`,
  `UnrollPhase`, `UnrollSweep`, `UnrollFailure`).

## Relationships

- **Depends on**: `vhtlcrecovery` (types + constants), `unroll`
  (EnsureUnrollRequest/Resp, GetStatusRequest/Resp, ExitPolicyKind,
  TriggerManual), `baselib/actor` (ActorRef for unroll registry).
- **Depended on by**: `darepod` (wiring), `swapclientserver` (arm/escalate
  on swap lifecycle events).
- **Sends**:
  - → `unroll` actor (via `ActorUnrollRegistry.Ask`):
    `EnsureUnrollRequest`, `GetStatusRequest`

## Invariants

- `Service` holds no durable in-memory state; all lifecycle state lives
  in the SQL `Store`.
- **Write before admit**: `EscalateRecovery` persists
  `StateUnrollStarted` in the DB before asking the unroll registry to
  admit the target. Crash-safe restart reissues admission idempotently.
- **Armed jobs stay dormant on restore**: `RestoreNonTerminal` only
  reissues escalated (non-armed) jobs; armed jobs require an explicit
  `EscalateRecovery` call.
- **Policy mismatch fails closed**: If the unroll registry already has a
  job for the same target with a different `exit_policy_kind` or
  `exit_policy_ref`, recovery fails with `errUnrollPolicyMismatch`.
- **Terminal reconciliation**: `GetRecoveryStatus` and
  `ListRecoveryStatuses` fold terminal unroll outcomes
  (`PhaseCompleted` / `PhaseFailed`) back into the durable row so
  callers see a unified terminal state.
- **Transient errors preserved**: If the unroll status probe fails after
  successful admission, the recovery row stays non-terminal so the next
  restore can retry.

## Deep Docs

- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
