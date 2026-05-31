# vhtlcrecovery/coordinator

## Purpose

Runtime coordinator for durable vHTLC recovery jobs. The package turns an armed
SQL recovery row into a generic unroll admission by passing
`(exit_policy_kind, recovery_id)` to the unroll registry.

This package exists as a child of `vhtlcrecovery` to avoid an import cycle:
`db` imports the parent package for row types, while the coordinator imports
`unroll` for admission and status.

## Key Types

- `Service` — arm/escalate/cancel/status coordinator. Thin service (not a
  durable actor); SQL owns recovery state.
- `ServiceConfig` — wires `Store`, `Unroll`, optional `Log`, and optional
  `TargetMaterializer`.
- `Store` — durable SQL persistence surface: ArmRecovery, GetRecovery,
  ListNonTerminalRecoveries, ListRecoveries, EscalateRecovery, CancelRecovery,
  CompleteRecovery, FailRecovery.
- `UnrollRegistry` — narrow unroll admission/status surface (EnsureUnroll +
  GetStatus). Narrower than the actor ref so tests can model admission without
  spinning up the full unroll subsystem.
- `ActorUnrollRegistry` — adapter from the live unroll registry actor ref to
  the `UnrollRegistry` interface.
- `TargetMaterializer` — optional domain adapter that prepares local VTXO/OOR
  artifacts for non-standard vHTLC targets before generic unroll admission.
- `RecoveryStatus` — durable recovery row joined with current unroll status
  (phase, sweep txid, active flag, failure reason).

## Relationships

- **Depends on**:
  - `vhtlcrecovery` (row types and state constants)
  - `unroll` (EnsureUnrollRequest, GetStatusResp, Phase constants)
  - `baselib/actor` (ActorRef for ActorUnrollRegistry adapter)
- **Depended on by**: `darepod` (wires the service at daemon startup)
- **Sends**:
  - → `unroll` registry actor (via `ActorUnrollRegistry`):
    `EnsureUnrollRequest`, `GetStatusRequest`
- **Receives**:
  - ← `darepod`: ArmRecovery, EscalateRecovery, CancelRecovery,
    GetRecoveryStatus, ListRecoveryStatuses, RestoreNonTerminal calls

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

- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
