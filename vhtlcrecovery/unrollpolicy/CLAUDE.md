# vhtlcrecovery/unrollpolicy

## Purpose

Adapts durable vHTLC recovery rows into `unroll.ExitSpendPolicy` objects.
Reconstructs all vHTLC script parameters from SQL columns, resolves
preimage secrets (from the recovery row for cross-process callers, or via
a registered swap-store resolver for in-process swap runtimes), and builds
fully signed exit transactions that satisfy both CSV and CLTV requirements.

## Key Types

- `VHTLCExitSpendPolicy` — Implements `unroll.ExitSpendPolicy`. Reconstructed
  from explicit recovery-row columns. Key methods: `Kind()`, `CSVDelay()`,
  `RequiredLockTime()`, `ValidateTarget(*wire.TxOut)`, `BuildSpendTx(ctx,
  ExitSpendRequest)`. Does not persist or broadcast; unroll owns those.
  Constructors: `NewClaimExitSpendPolicy`, `NewRefundWithoutReceiverExitSpendPolicy`.
- `ExitSpendPolicyResolver` — Implements `unroll.ExitSpendPolicyResolver`.
  Loads recovery rows by id and reconstructs the correct policy. Handles both
  `ExitPolicyKindClaim` and `ExitPolicyKindRefundWithoutReceiver`. Fields:
  `Jobs RecoveryJobLoader`, `Preimage PreimageResolver`.
- `PreimageResolverRegistry` — Concurrency-safe indirection for the
  optional swap-runtime preimage resolver. The unroll subsystem starts before
  the swap-runtime subserver; the registry holds a nil resolver initially and
  the subserver calls `SetResolver` during startup. Fails closed (returns error)
  if no resolver is registered when claim resolution is attempted.
- `RecoveryJobLoader` — `GetRecovery(ctx, id) (*vhtlcrecovery.RecoveryJob, error)`
- `PreimageResolver` — `ResolvePreimage(ctx, swapID []byte, preimageHash
  lntypes.Hash) (lntypes.Preimage, error)`. Must verify the returned preimage
  matches `preimageHash`.

## Relationships

- **Depends on**: `vhtlcrecovery` (recovery job types), `unroll` (ExitSpendPolicy
  interface), `lib/arkscript` (vHTLC script compilation), `lib/tx/arktx`
  (tx version), `keychain` (signing key derivation).
- **Depended on by**: `darepod` / `swapclientserver` (registers
  `ExitSpendPolicyResolver` with the unroll registry config, installs
  `PreimageResolverRegistry.SetResolver` during swap-runtime startup).

## Invariants

- Claim path requires preimage knowledge; `BuildSpendTx` resolves it via
  `resolveClaimPreimage` which tries the recovery row first, then the
  registered `PreimageResolver`.
- Refund-without-receiver path requires both the Ark CSV delay AND the vHTLC
  refund CLTV locktime; `BuildSpendTx` returns `unroll.ErrExitSpendNotMatured`
  if the chain height has not yet reached the required locktime.
- Fee rate is stored on the recovery row in sat/kw; `BuildSpendTx` receives
  sat/vB from unroll and converts (×250) before comparing to the cap.
- `ValidateTarget` fails closed if the materialized output's pkScript does not
  match the reconstructed vHTLC policy, preventing recovery on a wrong output.
- `estimatedVHTLCExitVBytes = 260` is a conservative estimate used for fee
  calculation before the exact signed tx is known.

## Deep Docs

- [vhtlcrecovery/CLAUDE.md](../CLAUDE.md) — Recovery types and constants.
- [unroll/CLAUDE.md](../../unroll/CLAUDE.md) — ExitSpendPolicy interface.
- [lib/arkscript/CLAUDE.md](../../lib/arkscript/CLAUDE.md) — vHTLC script policy.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
