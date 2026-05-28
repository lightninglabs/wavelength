# vhtlcrecovery/unrollpolicy

## Purpose

Adapter package that converts durable vHTLC recovery rows into concrete
`unroll.ExitSpendPolicy` implementations. Exists to avoid an import
cycle: `db` imports `vhtlcrecovery`, `unroll` imports `db`-facing
interfaces, so this adapter (importing both) must live below the parent
package.

## Key Types

- `ExitSpendPolicyResolver` — Implements `unroll.ExitSpendPolicyResolver`.
  Reconstructs vHTLC exit spend policies from durable recovery rows.
  - `SupportsKind(kind)` — advertises support for
    `ExitPolicyKindClaim` and `ExitPolicyKindRefundWithoutReceiver`.
  - `ResolveExitSpendPolicy(ctx, req)` — loads the recovery row, validates
    the kind matches, resolves the preimage (for claim), and returns a
    fully constructed `VHTLCExitSpendPolicy`.
- `VHTLCExitSpendPolicy` — Implements `unroll.ExitSpendPolicy`. Holds the
  recovery job, reconstructed `arkscript.VHTLCPolicy`, derived spend path,
  and key descriptor.
  - `NewClaimExitSpendPolicy(job, preimage)` — validates preimage against
    `PreimageHash` and derives the claim spend path.
  - `NewRefundWithoutReceiverExitSpendPolicy(job)` — derives the
    refund-without-receiver spend path satisfying both Ark CSV and vHTLC
    CLTV requirements.
  - `CSVDelay()`, `RequiredLockTime()` — per-action timing constraints.
  - `ValidateTarget(*wire.TxOut)` — verifies the materialized output
    matches the reconstructed vHTLC policy before building any spend.
  - `BuildSpendTx(ctx, req)` — constructs and signs the final v3 exit
    spend transaction; enforces fee cap and locktime maturity guard.
- `PreimageResolverRegistry` — Concurrency-safe indirection for the
  optional in-process swap runtime.
  - `SetResolver(PreimageResolver)` — install or replace (safe to pass
    nil to disable claim recovery).
  - `ResolvePreimage(ctx, swapID, hash)` — delegate to registered
    resolver; fails closed if nil.
- `RecoveryJobLoader` — Narrow store interface: `GetRecovery(ctx, id)`.
- `PreimageResolver` — Swap-owned secret: `ResolvePreimage(ctx, swapID,
  preimageHash) (Preimage, error)`.

## Relationships

- **Depends on**: `vhtlcrecovery` (types + constants), `unroll`
  (ExitSpendPolicy, ExitSpendPolicyResolver, ErrExitSpendNotMatured,
  ExitSpendRequest, ExitSpendPolicyRequest), `lib/arkscript` (VHTLCPolicy,
  CompiledPolicy, SpendPath), `lib/tx/arktx` (TxVersion).
- **Depended on by**: `darepod` (registers with the unroll registry at
  startup), `swapclientserver` (installs `PreimageResolver` via
  `SetResolver`).

## Invariants

- **Raw preimage never logged**: Both cross-process (`ClaimPreimage` field)
  and in-process (`PreimageResolver`) paths must never surface the
  preimage in logs. Claim policies do not write it back to the recovery
  row.
- **Target validation required before spend**: `ValidateTarget` confirms
  the materialized output matches the reconstructed vHTLC policy; mismatch
  fails closed before any signing.
- **CSV + CLTV for refund**: `RefundWithoutReceiverExitSpendPolicy` applies
  both the Ark CSV (`nSequence`) and the invoice CLTV (`nLockTime`).
- **Locktime maturity guard**: `BuildSpendTx` returns
  `ErrExitSpendNotMatured` when `CurrentHeight < RequiredLockTime` so the
  unroll FSM defers instead of broadcasting a non-final transaction.
- **Fee cap before signing**: `validateFeeRate` runs before any wallet key
  derivation or signing.
- **Preimage verified on construction**: `NewClaimExitSpendPolicy` checks
  `SHA256(preimage) == PreimageHash` before deriving the spend path.
- **Action-to-policy-kind mapping validated**: Both direct construction and
  resolver paths assert that `job.Action` matches `job.ExitPolicyKind`.

## Deep Docs

- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
