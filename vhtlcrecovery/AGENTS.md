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

- `RecoveryJob` — typed representation of one `vhtlc_recovery_jobs` row.
- `ActionClaim` / `ActionRefundWithoutReceiver` — recovery action constants
  for the two unilateral spend leaves.
- `ExitPolicyKindClaim` / `ExitPolicyKindRefundWithoutReceiver` — policy kind
  strings persisted on recovery jobs and passed to unroll.
- `StateArmed` / `StateUnrollStarted` / `StateCompleted` / `StateCancelled`
  / `StateFailed` — durable state machine constants for recovery lifecycle.
- `ExitPolicyKindForAction` — maps an action constant to its unroll exit
  policy kind string.

## Relationships

- **Depends on**: No local repo packages (intentionally minimal; pure domain
  types only).
- **Depended on by**: `db` (persistence: `VHTLCRecoveryStoreDB`),
  `vhtlcrecovery/coordinator` (control flow and state transitions),
  `vhtlcrecovery/unrollpolicy` (builds concrete exit spend policies from job
  rows), `waved` (RPC handlers: `ArmVHTLCRecovery`, `EscalateVHTLCRecovery`,
  etc.)

## Invariants

- The arm path never stores the raw preimage on the recovery row. Recovery
  stores only `preimage_hash` plus the swap reference, and the in-process
  claim path resolves the preimage from swap-owned state via
  `unrollpolicy.PreimageResolver`. A later execution-layer escalation may
  populate the nullable `claim_preimage` column only when the caller cannot
  rely on the daemon's registered swap store (e.g., cross-process recovery).
  The value must never be logged: any future Go view of the row that surfaces
  the preimage must redact it in `String` / `LogValue` so structured log
  expansion is safe.
- A recovery job is armed before it escalates. Armed jobs are dormant and
  should be cancelled when cooperative OOR succeeds.
- `exit_tx` bytes are persisted before broadcast in the later execution layer
  so restart replay reuses the same transaction.
- `failed` means manual attention may be required; `cancelled` means recovery
  was safely superseded by cooperative completion or explicit cancellation.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
