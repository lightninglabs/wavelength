# vhtlcrecovery

## Purpose

Durable control-plane types for vHTLC on-chain recovery. The package records
the recovery intent that higher-level swap FSMs arm before a cooperative path
fails, then lets lower layers resolve an explicit unroll exit policy by
`(exit_policy_kind, recovery_id)`.

The package is intentionally not an unroll implementation. `unroll` remains the
generic materialization and broadcast subsystem; vHTLC recovery owns the
vHTLC-specific action, script parameters, swap linkage, fee cap, and terminal
state.

## Key Types

- `RecoveryJob` - typed representation of one `vhtlc_recovery_jobs` row.
- `ActionClaim` - unilateral claim recovery for the side that owns the
  preimage.
- `ActionRefundWithoutReceiver` - unilateral sender refund recovery that does
  not require receiver cooperation.
- `ExitPolicyKindClaim` / `ExitPolicyKindRefundWithoutReceiver` - policy kinds
  persisted on recovery jobs and passed to unroll.

## Invariants

- The raw preimage is never stored on the recovery row and must never be logged.
  Recovery stores only `preimage_hash` plus the swap reference.
- A recovery job is armed before it escalates. Armed jobs are dormant and should
  be cancelled when cooperative OOR succeeds.
- `exit_tx` bytes are persisted before broadcast in the later execution layer so
  restart replay reuses the same transaction.
- `failed` means manual attention may be required; `cancelled` means recovery
  was safely superseded by cooperative completion or explicit cancellation.
