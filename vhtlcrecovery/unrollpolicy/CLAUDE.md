# vhtlcrecovery/unrollpolicy

## Purpose

Adapts durable vHTLC recovery rows into `unroll.ExitSpendPolicy` values.
The generic unroll subsystem treats exit spend as a pluggable strategy; this
package is the vHTLC-specific plugin that reconstructs the correct tapscript
spend path from recovery-row columns and signs the final exit transaction.

## Key Types

- `VHTLCExitSpendPolicy` — Implements `unroll.ExitSpendPolicy`. Reconstructed
  from a `RecoveryJob` at policy-resolve time; holds the compiled `arkscript.VHTLCPolicy`,
  the selected `SpendPath`, and the signing key descriptor. Does not persist
  or broadcast transactions — unroll owns those restart-safety responsibilities.
- `ExitSpendPolicyResolver` — Implements `unroll.ExitSpendPolicyResolver` and
  `unroll.ResolverKindSupport`. Loads the recovery row by ref, validates the
  kind matches, and constructs the concrete policy. For claim policies it also
  resolves the preimage via `PreimageResolver` (or the durable row field for
  cross-process callers).
- `PreimageResolverRegistry` — Concurrency-safe indirection for the
  daemon's optional swap runtime. The unroll subsystem starts before the
  `swapruntime` subserver registers its swap store; this registry bridges the
  initialization gap. Fails closed if no resolver is installed when a claim
  recovery needs the preimage.
- `RecoveryJobLoader` interface — `GetRecovery(ctx, id)`. Provided by `db`.
- `PreimageResolver` interface — `ResolvePreimage(ctx, swapID, preimageHash)`.
  Provided by the swap runtime, or cross-process callers pass the preimage
  directly via `RecoveryJob.ClaimPreimage`.
- `NewClaimExitSpendPolicy(job, preimage)` — Constructs a claim exit policy
  after verifying the preimage matches the recovery row's hash.
- `NewRefundWithoutReceiverExitSpendPolicy(job)` — Constructs a sender-only
  refund policy; no preimage needed.

## Relationships

- **Depends on**: `vhtlcrecovery` (recovery row types and exit policy kind
  constants), `unroll` (`ExitSpendPolicy`, `ExitSpendPolicyResolver`,
  `ResolverKindSupport`, `ExitSpendRequest`), `lib/arkscript` (vHTLC policy
  construction and script-path spend helpers).
- **Depended on by**: `darepod` (wires `ExitSpendPolicyResolver` into the
  unroll registry's resolver chain and installs the
  `PreimageResolverRegistry`; the `swapruntime` subserver calls
  `SetResolver` once its swap store is ready).

## Invariants

- `VHTLCExitSpendPolicy.BuildSpendTx` validates `RequiredLockTime` against
  `req.CurrentHeight` and returns `unroll.ErrExitSpendNotMatured` when the
  transaction would be non-final. The unroll FSM retries on this error until
  height catches up rather than burning broadcast attempts.
- The raw preimage is only held in-memory in the spend path's witness
  material. It is NOT written back to the recovery row during
  `BuildSpendTx`.
- Claim action signs with `ReceiverPubkey`; `ActionRefundWithoutReceiver`
  signs with `SenderPubkey`. `signingKey` enforces the action-to-kind
  mapping and panics on unknown actions rather than silently producing a
  wrong signature.
- `ExitSpendPolicyResolver.SupportsKind` reports true only for
  `ExitPolicyKindClaim` and `ExitPolicyKindRefundWithoutReceiver`. The
  unroll registry uses this at boot to detect orphaned jobs whose kind no
  registered resolver covers.
- `policyFromJob` converts all `int32` SQL fields to `uint32` with
  positivity checks before calling `arkscript.NewVHTLCPolicy`, preventing
  wraparound on negative SQL values.

## Deep Docs

- [vhtlcrecovery/CLAUDE.md](../CLAUDE.md) — Recovery row data types.
- [vhtlcrecovery/coordinator/CLAUDE.md](../coordinator/CLAUDE.md) — Service
  coordinating recovery with unroll.
- [unroll/CLAUDE.md](../../unroll/CLAUDE.md) — Generic unroll registry and
  exit policy interfaces.
- [lib/arkscript/CLAUDE.md](../../lib/arkscript/CLAUDE.md) — vHTLC tapscript
  policy construction.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
