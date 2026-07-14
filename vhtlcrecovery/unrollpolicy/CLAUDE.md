# vhtlcrecovery/unrollpolicy

## Purpose

Adapter package that turns durable vHTLC recovery rows into concrete
`unroll.ExitSpendPolicy` implementations. The parent `vhtlcrecovery` package
owns the durable control-plane types, while `unroll` owns generic VTXO
materialization and final-spend execution.

This subpackage exists to keep that boundary explicit without introducing an
import cycle: `db` imports `vhtlcrecovery`, and `unroll` imports `db`, so the
adapter that imports both `unroll` and `vhtlcrecovery` must live below the
parent package.

## Key Types

- `ExitSpendPolicyResolver` — resolves an unroll `(exit_policy_kind,
  exit_policy_ref)` pair into a vHTLC-specific exit policy. Registered into
  `unroll.RegistryConfig.ExitSpendPolicyResolver` at daemon startup.
- `VHTLCExitSpendPolicy` — validates the materialized vHTLC target output and
  builds the final claim or refund-without-receiver spend.
- `RecoveryJobLoader` — narrow store interface for loading a recovery job by
  id.
- `PreimageResolver` — narrow interface for loading the swap-owned preimage
  when claim recovery needs it.
- `PreimageResolverRegistry` — concurrency-safe indirection point for the
  daemon's optional swap runtime. Holds the concrete resolver installed by
  the swap subserver after startup.

## Relationships

- **Depends on**: `vhtlcrecovery` (RecoveryJob types and state constants),
  `unroll` (ExitSpendPolicy interface, `ErrExitSpendNotMatured`),
  `lib/arkscript` (vHTLC policy template construction), `lib/tx/arktx`
  (transaction version for sweep tx).
- **Depended on by**: `waved` (constructs `ExitSpendPolicyResolver`,
  registers into `unroll.RegistryConfig`, installs `PreimageResolver` via
  `PreimageResolverRegistry.SetResolver` at swap-runtime startup).

## Invariants

- The raw preimage must never be logged. Claim recovery first uses durable
  `claim_preimage` when cross-process escalation supplied it, otherwise it
  resolves from the swap-owned in-process preimage resolver and checks it
  against `preimage_hash` before building a spend.
- `ValidateTarget` must confirm the materialized output matches the recovered
  vHTLC policy before any final spend is built.
- `refund_without_receiver` spends must carry both Ark CSV (`nSequence`) and
  invoice/vHTLC CLTV (`nLockTime`).
- `RequiredLockTime` reports the absolute nLockTime each policy demands.
  `BuildSpendTx` refuses to construct a tx when the caller-supplied
  `ExitSpendRequest.CurrentHeight` has not reached `RequiredLockTime`; the
  failure is signaled with `unroll.ErrExitSpendNotMatured` so the unroll FSM
  can defer broadcast instead of looping on a non-final transaction.
- The wrapping VTXO descriptor's `RelativeExpiry` must be greater than or
  equal to the policy's `CSVDelay` for the leaf being spent. Violated invariant
  causes the unroll actor to fail fast.
- Fee-cap checks happen before building the signed final spend. Persistence and
  broadcast remain owned by `unroll`.

## Deep Docs

- [vhtlcrecovery/CLAUDE.md](../CLAUDE.md) — Parent package: durable types.
- [unroll/CLAUDE.md](../../unroll/CLAUDE.md) — Generic unroll subsystem.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
