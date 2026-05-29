# vhtlcrecovery/coordinator

## Purpose

Runtime coordinator for durable vHTLC recovery jobs. The package turns an armed
SQL recovery row into a generic unroll admission by passing
`(exit_policy_kind, recovery_id)` to the unroll registry.

This package exists as a child of `vhtlcrecovery` to avoid an import cycle:
`db` imports the parent package for row types, while the coordinator imports
`unroll` for admission and status.

## Key Types

- `Service` - arm/escalate/cancel/status coordinator.
- `Store` - durable SQL persistence surface used by the service.
- `UnrollRegistry` - narrow unroll admission/status surface.
- `ActorUnrollRegistry` - adapter from the live unroll registry actor to the
  narrow service interface.
- `RecoveryStatus` - durable recovery row joined with current unroll status.

## Relationships

- **Depends on**: `vhtlcrecovery` (RecoveryJob row types),
  `unroll` (admission registry), `db` (Store implementation).
- **Depended on by**: `darepod` (wires Service into the daemon, exposes via
  ArmVHTLCRecovery / EscalateVHTLCRecovery / CancelVHTLCRecovery RPCs).

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
