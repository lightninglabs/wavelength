# vhtlcrecovery/coordinator

## Purpose

Runtime coordinator for durable vHTLC recovery jobs. The package turns an armed
SQL recovery row into a generic unroll admission by passing
`(exit_policy_kind, recovery_id)` to the unroll registry.

This package exists as a child of `vhtlcrecovery` to avoid an import cycle:
`db` imports the parent package for row types, while the coordinator imports
`unroll` for admission and status.

## Key Types

- `Service` — Arm/escalate/cancel/status coordinator. Thin service backed
  entirely by SQL state; no durable in-memory state.
- `ServiceConfig` — Wires `Store`, `UnrollRegistry`, optional `Log`, and
  optional `TargetMaterializer`.
- `Store` — Durable SQL persistence surface: `ArmRecovery`, `GetRecovery`,
  `ListNonTerminalRecoveries`, `ListRecoveries`, `EscalateRecovery`,
  `CancelRecovery`, `CompleteRecovery`, `FailRecovery`.
- `UnrollRegistry` — Narrow unroll admission/status surface: `EnsureUnroll`,
  `GetStatus`.
- `ActorUnrollRegistry` — Adapter from the live `unroll.RegistryMsg` actor
  ref to the narrow `UnrollRegistry` interface.
- `TargetMaterializer` — Optional hook called before admission to prepare
  local VTXO/package state for non-standard vHTLC targets.
- `RecoveryStatus` — Durable recovery row joined with current unroll status
  observations.

## Relationships

- **Depends on**: `vhtlcrecovery` (row types, state/action/policy constants),
  `unroll` (`EnsureUnrollRequest`, `GetStatusResp`, `Phase`,
  `RegistryMsg`/`RegistryResp`), `baselib/actor` (actor ref for
  `ActorUnrollRegistry`), `db` (production `Store` implementation lives in
  `db`).
- **Depended on by**: `darepod` (wires the service, exposes vHTLC recovery
  RPCs via `rpc_vhtlc_recovery.go`).
- **Sends**:
  - → `unroll` registry (Ask via `ActorUnrollRegistry`): `EnsureUnrollRequest`,
    `GetStatusRequest`.
- **Receives**:
  - ← `darepod` (direct method calls): `ArmRecovery`, `EscalateRecovery`,
    `CancelRecovery`, `GetRecoveryStatus`, `ListRecoveryStatuses`,
    `RestoreNonTerminal`.

## Invariants

- Recovery state is SQL-owned. The service keeps no durable in-memory state.
- Escalation writes `unroll_started` before asking unroll to admit the target,
  so restart can reissue admission if the process dies during the handoff.
- Armed jobs are dormant. `RestoreNonTerminal` only reissues jobs that had
  already escalated before shutdown.
- Any existing unroll job for the same target must carry the same
  `exit_policy_kind` and `exit_policy_ref`; mismatches fail closed with
  `errUnrollPolicyMismatch` and the recovery row is marked `failed`.
- The raw preimage is not present in this package. Claim policies resolve it
  later through the policy adapter.
- `validateClaimPreimage` verifies the cross-process preimage against the stored
  hash before writing it to the row; nil is accepted (in-process resolver path).

## Deep Docs

- [vhtlcrecovery/CLAUDE.md](../CLAUDE.md) — Parent package control-plane
  types.
- [unroll/CLAUDE.md](../../unroll/CLAUDE.md) — Generic unroll registry and
  exit spend policy model.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
