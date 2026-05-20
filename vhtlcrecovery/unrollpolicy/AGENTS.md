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

- `ExitSpendPolicyResolver` - resolves an unroll `(exit_policy_kind,
  exit_policy_ref)` pair into a vHTLC-specific exit policy.
- `VHTLCExitSpendPolicy` - validates the materialized vHTLC target output and
  builds the final claim or refund-without-receiver spend.
- `RecoveryJobLoader` - narrow store interface for loading a recovery job by
  id.
- `PreimageResolver` - narrow interface for loading the swap-owned preimage
  when claim recovery needs it.

## Invariants

- The raw preimage is not stored on the recovery row. Claim recovery resolves it
  from the swap-owned state and checks it against `preimage_hash` before
  building a spend.
- `ValidateTarget` must confirm the materialized output matches the recovered
  vHTLC policy before any final spend is built.
- `refund_without_receiver` spends must carry both Ark CSV (`nSequence`) and
  invoice/vHTLC CLTV (`nLockTime`).
- Fee-cap checks happen before building the signed final spend. Persistence and
  broadcast remain owned by `unroll`.
