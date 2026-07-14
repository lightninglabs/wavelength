# vhtlcrecovery/coordinator

## Purpose

Runtime coordinator for durable vHTLC recovery jobs. The package turns an armed
SQL recovery row into a unilateral exit by forcing the target through the VTXO
manager's admission gate under the recovery row's
`(exit_policy_kind, recovery_id)` identity. The manager owns the state
transition (persisting the target out of the live set) and starts the durable
unroll job through its chain-resolver seam, so a vHTLC exit converges on the
same path as manual, critical-expiry, and fraud exits.

This package exists as a child of `vhtlcrecovery` to avoid an import cycle:
`db` imports the parent package for row types, while the coordinator forces the
exit through the VTXO manager (`actormsg`) and reads status back from `unroll`.

## Key Types

- `Service` — arm/escalate/cancel/status coordinator.
- `Store` — durable SQL persistence surface used by the service.
- `UnrollRegistry` — narrow unroll status surface (narrower than the actor ref
  so tests can model status without spinning up the full unroll subsystem).
  Admission no longer lives here; recovery only reads status back.
- `ExitAdmitter` — forces a recovery target into unilateral exit through the
  VTXO manager's single admission gate via `ForceExit`. The manager owns the
  transition and starts the registry job through its chain-resolver seam.
- `ActorUnrollRegistry` — adapter from the live unroll registry actor to the
  narrow status interface.
- `TargetMaterializer` — adapter interface for ensuring the vHTLC target has
  local descriptor and package bindings that generic unroll needs. Implemented
  by `waved.vhtlcRecoveryTargetMaterializer`.
- `RecoveryStatus` — durable recovery row joined with current unroll status.
- `ServiceConfig` — wiring: `Store`, `UnrollRegistry`, `ExitAdmitter`,
  `TargetMaterializer`.

## Relationships

- **Depends on**: `vhtlcrecovery` (row types and state constants), `lib/actormsg`
  (exit admission: `ForceUnrollRequest`, `ExitPolicy`), `unroll` (status:
  `GetStatusRequest`, `GetStatusResp`), `baselib/actor` (actor refs for
  `ActorUnrollRegistry`).
- **Depended on by**: `waved` (instantiates and wires the service; supplies
  the VTXO manager as the `ExitAdmitter` and implements `TargetMaterializer`
  via `vhtlcRecoveryTargetMaterializer`).
- **Messages to/from**: Sends `ForceUnrollRequest` -> VTXO manager (via
  `ExitAdmitter`) to admit the exit, and `GetStatusRequest` -> `unroll`
  registry (via `UnrollRegistry`) to read status back. `Service` methods
  (`ArmRecovery`, `EscalateRecovery`, `CancelRecovery`, `GetRecoveryStatus`,
  `ListRecoveryStatuses`, `RestoreNonTerminal`) are called directly by
  `waved.RPCServer`, not actor messages.

## Invariants

- Recovery state is SQL-owned. The service keeps no durable in-memory state.
- Escalation writes `unroll_started` before forcing the exit through the VTXO
  manager, so restart can reissue admission if the process dies during the
  handoff.
- Armed jobs are dormant. `RestoreNonTerminal` only reissues jobs that had
  already escalated before shutdown.
- `ExitAdmitter.ForceExit` returns once the manager has transitioned the VTXO
  to unilateral exit, but the registry job is started asynchronously through
  the manager's outbox. The service does not read the registry record back for
  synchronous verification: a not-yet-visible record is the normal case, left
  to the registry's own admission-boundary validation and the restart re-drive.
- A best-effort policy-conflict guard reads status back after forcing the exit:
  if an existing unroll job already claimed the target under a different
  `exit_policy_kind`, the recovery fails closed rather than exit under the
  wrong policy.
- `EscalateRecovery` accepts an optional raw claim preimage, validates it
  against the job's `preimage_hash`, then hands it to `Store.EscalateRecovery`
  for persistence, but never logs it (`recoveryLogAttrs` omits `ClaimPreimage`
  deliberately).

## Deep Docs

- [vhtlcrecovery/CLAUDE.md](../CLAUDE.md) — Parent package: durable types.
- [unroll/CLAUDE.md](../../unroll/CLAUDE.md) — Generic unroll subsystem.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
