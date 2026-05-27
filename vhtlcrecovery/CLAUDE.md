# vhtlcrecovery

## Purpose

Defines durable vHTLC on-chain recovery data types and constants. This root
package owns the persistent row representation stored in SQL and the FSM state
constants used by the coordinator and database layers. It contains no runtime
logic — callers import it to share types without creating import cycles.

## Key Types

- `RecoveryJob` — Durable control-plane row with 32 fields covering identity,
  on-chain context, cryptographic parameters, unroll linkage, exit artifacts,
  and timestamped lifecycle transitions. `IsTerminal()` reports whether the job
  has reached a terminal state.
- Direction constants — `DirectionPay`, `DirectionReceive`,
  `DirectionServerIn`, `DirectionServerOut` identify which party owns the
  recovery job.
- Action constants — `ActionClaim` (preimage spend), `ActionRefundWithoutReceiver`
  (sender-only refund) name the two unilateral exit paths.
- FSM state constants — `StateArmed` through `StateFailed` (11 states) covering
  the full recovery lifecycle from dormant intent to terminal outcome.
- Exit policy kind constants — `ExitPolicyKindClaim`,
  `ExitPolicyKindRefundWithoutReceiver` map recovery actions to their unroll
  exit policy identities.
- `ExitPolicyKindForAction(action string) (string, error)` — Maps a recovery
  action to its corresponding `ExitPolicyKind`.

## Relationships

- **Depends on**: `btcd/wire` (outpoint and tx types only).
- **Depended on by**: `db` (SQL persistence), `vhtlcrecovery/coordinator`
  (runtime coordination), `vhtlcrecovery/unrollpolicy` (exit policy
  construction).

## Invariants

- `RecoveryJob.RefundLocktime` is stored as `int32` for SQL compatibility but
  represents an unsigned Bitcoin locktime; the unrollpolicy layer validates and
  converts before use.
- The action and exit-policy-kind namespaces are intentionally separate so
  callers must go through `ExitPolicyKindForAction` rather than guessing
  a string equality.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
