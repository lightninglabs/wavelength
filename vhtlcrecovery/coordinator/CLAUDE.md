# vhtlcrecovery/coordinator

## Purpose

Runtime coordinator for durable vHTLC recovery jobs. The package turns an armed
SQL recovery row into a generic unroll admission by passing
`(exit_policy_kind, recovery_id)` to the unroll registry.

This package exists as a child of `vhtlcrecovery` to avoid an import cycle:
`db` imports the parent package for row types, while the coordinator imports
`unroll` for admission and status.

## Key Types

- `Service` — arm/escalate/cancel/status coordinator.
- `Store` — durable SQL persistence surface used by the service.
- `UnrollRegistry` — narrow unroll admission/status surface (narrower than the
  actor ref so tests can model admission without spinning up the full unroll
  subsystem).
- `ActorUnrollRegistry` — adapter from the live unroll registry actor to the
  narrow service interface.
- `TargetMaterializer` — adapter interface for ensuring the vHTLC target has
  local descriptor and package bindings that generic unroll needs. Implemented
  by `darepod.vhtlcRecoveryTargetMaterializer`.
- `RecoveryStatus` — durable recovery row joined with current unroll status.
- `ServiceConfig` — wiring: `Store`, `UnrollRegistry`, `TargetMaterializer`.

## Relationships

- **Depends on**: `vhtlcrecovery` (row types and state constants), `unroll`
  (admission/status: `EnsureUnrollRequest`, `EnsureUnrollResp`,
  `GetStatusRequest`, `GetStatusResp`), `baselib/actor` (actor refs for
  `ActorUnrollRegistry`).
- **Depended on by**: `darepod` (instantiates and wires the service; implements
  `TargetMaterializer` via `vhtlcRecoveryTargetMaterializer`).
- **Sends**:
  - → `unroll` registry: `EnsureUnrollRequest`, `GetStatusRequest`
- **Receives**:
  - ← API: `ArmRecovery`, `EscalateRecovery`, `CancelRecovery`,
    `CompleteRecovery`, `FailRecovery`, `GetStatus`, `ListRecoveries`
    (from `darepod.RPCServer` via `Service`)

## Invariants

- Recovery state is SQL-owned. The service keeps no durable in-memory state.
- Escalation writes `unroll_started` before asking unroll to admit the target,
  so restart can reissue admission if the process dies during the handoff.
- Armed jobs are dormant. `RestoreNonTerminal` only reissues jobs that had
  already escalated before shutdown.
- Any existing unroll job for the same target must carry the same
  `exit_policy_kind` and `exit_policy_ref`; mismatches fail closed.
- The raw preimage is not present in this package. Claim policies resolve it
  later through the policy adapter.

## Deep Docs

- [vhtlcrecovery/CLAUDE.md](../CLAUDE.md) — Parent package: durable types.
- [unroll/CLAUDE.md](../../unroll/CLAUDE.md) — Generic unroll subsystem.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
