# vhtlcrecovery/coordinator

## Purpose

Bridges durable vHTLC recovery rows (owned by SQL) with the generic unroll
subsystem. The `Service` is a thin stateless coordinator — it is NOT a durable
actor. SQL owns recovery state; the unroll registry owns per-target execution.
This package lives as a child of `vhtlcrecovery` specifically so it can import
`unroll` without creating an import cycle.

## Key Types

- `Service` — Coordinates durable vHTLC recovery jobs with the generic unroll
  subsystem. Entry points: `ArmRecovery`, `EscalateRecovery`, `CancelRecovery`,
  `GetRecoveryStatus`, `ListRecoveryStatuses`, `RestoreNonTerminal`.
- `ServiceConfig` — Wires the service: `Store`, `Unroll`, `TargetMaterializer`,
  optional `Log`.
- `RecoveryStatus` — Joined view: the durable `RecoveryJob` plus best-effort
  runtime unroll observations (`UnrollFound`, `UnrollActive`, `UnrollPhase`,
  `UnrollSweep`, `UnrollFailure`). Unroll fields are absent while a job is
  armed.
- `Store` — Durable persistence surface. Methods: `ArmRecovery`, `GetRecovery`,
  `ListNonTerminalRecoveries`, `ListRecoveries`, `EscalateRecovery`,
  `CancelRecovery`, `CompleteRecovery`, `FailRecovery`. Implementations must use
  SQL transactions for every state mutation.
- `UnrollRegistry` — Narrow unroll control surface: `EnsureUnroll`,
  `GetStatus`. Narrower than the actor ref so tests can stub without spinning up
  the full unroll subsystem.
- `TargetMaterializer` — `EnsureRecoveryTarget(ctx, RecoveryJob) error`. Domain
  adapter that ensures the vHTLC target has descriptor and package bindings
  before unroll admission. Must be idempotent.
- `ActorUnrollRegistry` — Adapts the live `unroll.UnrollRegistryActor` ref to
  the `UnrollRegistry` interface via Ask/Await.

## Relationships

- **Depends on**: `vhtlcrecovery` (pure types), `unroll` (registry and policy
  types), `baselib/actor` (ActorRef for the live registry adapter).
- **Depended on by**: `darepod` / `swapclientserver` (wires Service into the
  daemon and swap-client runtime).
- **Sends**:
  - → `unroll` registry (Ask via `ActorUnrollRegistry`): `EnsureUnrollRequest`,
    `GetStatusRequest`.
- **Receives**:
  - ← API: `ArmRecovery`, `EscalateRecovery`, `CancelRecovery`,
    `GetRecoveryStatus`, `ListRecoveryStatuses`, `RestoreNonTerminal`.

## Invariants

- SQL transition happens **before** unroll admission in `EscalateRecovery` so a
  crash in the handoff is recovered by `RestoreNonTerminal` on daemon restart.
- `RestoreNonTerminal` reissues unroll admission only for jobs that were already
  escalated before the crash; armed jobs are left dormant.
- Cancellation does not stop an already-admitted unroll worker — it marks the
  recovery row and lets unroll finish or time out naturally.
- Policy mismatch (same outpoint claimed by unroll with a different exit policy)
  fails the recovery closed rather than silently admitting a conflicting policy.

## Deep Docs

- [vhtlcrecovery/CLAUDE.md](../CLAUDE.md) — Parent types and state constants.
- [unroll/CLAUDE.md](../../unroll/CLAUDE.md) — Generic unroll subsystem.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
