# vhtlcrecovery

## Purpose

Pure control-plane types for durable vHTLC on-chain recovery. Records
recovery intent before the cooperative path fails and provides identity
for unroll exit policies via an `(exit_policy_kind, recovery_id)` tuple.
Not an unroll implementation itself — generic unroll logic stays in the
`unroll` package.

## Key Types

- `RecoveryJob` — Core durable row for one on-chain recovery attempt.
  Carries identity (`ID`, `RequestID`, `SwapID`), lifecycle (`Direction`,
  `Action`, `State`), on-chain parameters (`VTXOOutpoint`,
  `VTXOAmountSat`, `DestinationScript`, `MaxFeeRateSatPerKWeight`),
  vHTLC script parameters (`SenderPubkey`, `ReceiverPubkey`,
  `ServerPubkey`, `PreimageHash`, delays, locktime), signer key locator,
  optional cross-process `ClaimPreimage` (never logged), unroll linkage
  (`UnrollTargetOutpoint`, `ExitPolicyKind`), and terminal fields
  (`ExitTx`, `ExitTxid`, `CooperativeTxid`, `LastError`,
  `CancelReason`).
- `Action*` constants — `ActionClaim` (preimage-owning side unilateral
  claim) and `ActionRefundWithoutReceiver` (sender refund without
  receiver cooperation).
- `Direction*` constants — `DirectionPay`, `DirectionReceive`,
  `DirectionServerIn`, `DirectionServerOut` — role in the recovery.
- `State*` constants — `StateArmed` → `StateUnrollStarted` →
  `StateWaitingForTarget` → `StateWaitingForCSV` →
  `StateBuildingExitSpend` → `StateExitSpendBuilt` →
  `StateSubmittingExitSpend` → `StateExitSpendPendingConfirmation` →
  `StateCompleted` / `StateCancelled` / `StateFailed`.
- `ExitPolicyKind*` constants — `ExitPolicyKindClaim` and
  `ExitPolicyKindRefundWithoutReceiver` — matched to `unroll` exit policy
  kind strings.
- `ExitPolicyKindForAction(action) (string, error)` — maps a recovery
  action constant to the corresponding unroll exit policy kind string.
- `RecoveryJob.IsTerminal() bool` — true when state is Completed,
  Cancelled, or Failed.

## Relationships

- **Depends on**: nothing in this repo (pure leaf type package).
- **Depended on by**: `db` (persistence), `vhtlcrecovery/coordinator`
  (runtime logic), `vhtlcrecovery/unrollpolicy` (spend policy adapter).

## Invariants

- This package holds only types and constants; no runtime logic, actors,
  or DB calls.
- `ClaimPreimage` is optional cross-process secret material and must
  never appear in logs. The field is nil for armed jobs and for
  receive-side jobs that resolve the preimage in-process.
- `PreimageHash` is always stored; raw preimage is never the canonical
  recovery identity.
- Failed state means the recovery needs operator attention. Cancelled
  means it was safely superseded (cooperative completion or explicit
  stop).
- One `RecoveryJob` covers exactly one direction; payer and receiver
  each own a separate row.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
