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

- `RecoveryJob` — Typed representation of one `vhtlc_recovery_jobs` row.
  Carries VTXO outpoint, script parameters, swap reference, direction, action,
  state, fee cap, and timestamps. `ClaimPreimage` is nullable and must never
  be logged.
- `ActionClaim` / `ActionRefundWithoutReceiver` — Action constants
  identifying which leaf the recovery targets.
- `ExitPolicyKindClaim` / `ExitPolicyKindRefundWithoutReceiver` — Policy
  kind strings passed to the unroll registry's `ExitPolicyKind` field.
- State constants: `StateArmed`, `StateUnrollStarted`, `StateWaitingForTarget`,
  `StateWaitingForCSV`, `StateBuildingExitSpend`, `StateExitSpendBuilt`,
  `StateSubmittingExitSpend`, `StateExitSpendPendingConfirmation`,
  `StateCompleted`, `StateCancelled`, `StateFailed`.
- `ExitPolicyKindForAction(action)` — Maps an action string to its unroll exit
  policy kind.

## Relationships

- **Depends on**: `btcd/wire` (for `wire.OutPoint` on `RecoveryJob`). No
  import of other repo packages — intentional to avoid cycles; `db` imports
  this package for the row type.
- **Depended on by**: `db` (persistence row type), `vhtlcrecovery/coordinator`
  (runtime service), `vhtlcrecovery/unrollpolicy` (exit spend policy adapter),
  `darepod` (vHTLC recovery RPC handlers).

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
