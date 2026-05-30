# vhtlcrecovery

## Purpose

Defines durable vHTLC on-chain recovery state. Pure data types used by the
`db` persistence layer and the `coordinator` runtime; no business logic lives
here. The split keeps the persistence layer free of `unroll` imports.

## Key Types

- `RecoveryJob` — Durable control-plane row for one vHTLC recovery intent.
  Holds all vHTLC script parameters (`SenderPubkey`, `ReceiverPubkey`,
  `ServerPubkey`, `PreimageHash`, `RefundLocktime`, all delay fields),
  the chosen `Action`, current `State`, `VTXOOutpoint`, fee cap, and
  optional cross-process `ClaimPreimage`. Must never be logged in full
  because `ClaimPreimage` is secret witness material.
- `IsTerminal() bool` — Reports whether the job has reached a terminal
  state (`completed`, `cancelled`, or `failed`).
- Direction constants — `DirectionPay`, `DirectionReceive`,
  `DirectionServerIn`, `DirectionServerOut`.
- Action constants — `ActionClaim` (spends the unilateral claim leaf using
  the preimage), `ActionRefundWithoutReceiver` (spends the unilateral
  refund leaf without receiver cooperation).
- State constants — ordered lifecycle phases from `StateArmed` through
  `StateUnrollStarted`, `StateWaitingForTarget`, `StateWaitingForCSV`,
  `StateBuildingExitSpend`, `StateExitSpendBuilt`,
  `StateSubmittingExitSpend`, `StateExitSpendPendingConfirmation` to the
  terminal `StateCompleted`, `StateCancelled`, `StateFailed`.
- Exit policy kind constants — `ExitPolicyKindClaim`,
  `ExitPolicyKindRefundWithoutReceiver`. Used as the durable policy
  identity stored on both the recovery row and the unroll job.
- `ExitPolicyKindForAction(action) (string, error)` — Maps a recovery
  action string to its corresponding `ExitPolicyKind`.

## Relationships

- **Depends on**: `github.com/btcsuite/btcd/wire` (OutPoint, MsgTx
  field types only). No internal repo imports — intentionally a leaf
  package to prevent import cycles.
- **Depended on by**: `db` (persistence), `vhtlcrecovery/coordinator`
  (runtime), `vhtlcrecovery/unrollpolicy` (exit spend policy adapter).

## Invariants

- `RecoveryJob.ClaimPreimage` must **never** be logged. It is raw secret
  witness material; the `recoveryLogAttrs` helper in `coordinator` omits
  it deliberately.
- `RefundLocktime` is stored as `int32` to match sqlc's SQLite integer
  mapping. Policy construction must range-check it to `>0` and convert to
  `uint32` before passing to Bitcoin script logic.
- All delay fields (`UnilateralClaimDelay`, `UnilateralRefundDelay`,
  `UnilateralRefundWithoutReceiverDelay`) are stored as `int32` for the
  same SQLite reason and must be validated positive before use.

## Deep Docs

- [vhtlcrecovery/coordinator/CLAUDE.md](coordinator/CLAUDE.md) — Runtime
  service coordinating recovery with unroll.
- [vhtlcrecovery/unrollpolicy/CLAUDE.md](unrollpolicy/CLAUDE.md) — Exit
  spend policy adapter for the unroll subsystem.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
