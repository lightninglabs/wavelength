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
  can defer broadcast instead of looping on a non-final transaction. The
  unroll actor mirrors this check before any wallet-pkScript derivation so a
  stalled refund never burns a fresh BIP32 address per block.
- The wrapping VTXO descriptor's `RelativeExpiry` (a.k.a. `proof.CSVDelay`)
  must be greater than or equal to the policy's `CSVDelay` for the leaf the
  policy is about to spend. When this invariant is violated, the unroll
  actor fails the build fast — broadcasting before the policy CSV is
  satisfied would produce a `non-BIP68-final` transaction that `txconfirm`
  would re-broadcast indefinitely.
- Fee-cap checks happen before building the signed final spend. Persistence and
  broadcast remain owned by `unroll`.
