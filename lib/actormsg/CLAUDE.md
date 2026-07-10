# lib/actormsg

## Purpose

Shared actor message marker interfaces and concrete admission types that cross
package boundaries. Lives in `lib/` to break import cycles between `vtxo`,
`round`, and `wallet` (which would otherwise create circular dependencies).

## Key Types

- `RoundReceivable` — Marker interface for messages the round actor can receive (used by `vtxo` and `wallet` to send to round without importing `round`).
- `RoundActorResp` — Marker interface for round actor responses.
- `VTXOManagerMsg` / `VTXOManagerResp` — Marker interfaces for VTXO manager messages and responses.
- `VTXOActorMsg` / `VTXOActorResp` — Marker interfaces for per-VTXO actor messages and responses.
- `SelectAndReserveSpendRequest` / `SelectAndReserveSpendResponse` — Ask-message to select and lock VTXOs for OOR spend.
- `SelectAndReserveForfeitRequest` / `SelectAndReserveForfeitResponse` — Ask-message to atomically select and reserve VTXOs for cooperative forfeit (directed sends). Combines coin selection and PendingForfeit reservation in one step to close a race window.
- `ReserveForfeitRequest` / `ReleaseForfeitRequest` — Forfeit reservation admission messages.
- `ReleaseSpendRequest` / `CompleteSpendRequest` — Spend lifecycle completion messages.
- `ForceUnrollRequest` / `ForceUnrollResponse` — Ask-message that routes an operator or chain-resolver unroll trigger through the VTXO manager into the per-VTXO FSM. `ForceUnrollRequest.Trigger` (a `UnrollTrigger`) names *why* the coin is exiting, and `ForceUnrollRequest.ExitPolicy` (an `fn.Option[ExitPolicy]`) names *which* exit-spend policy the target unrolls under, so a single admission path carries manual, critical-expiry, fraud, and vHTLC-recovery intent through to the unroll registry. `ForceUnrollResponse.Accepted` is true when the request caused a state transition; when false, `Reason` distinguishes `"no such vtxo"` from `"already terminal"` so callers don't misread a silent self-loop as success.
- `UnrollTrigger` — String-typed enum naming why a unilateral exit was started (`UnrollTriggerCriticalExpiry` is the empty-string zero value and preserves the historical critical-expiry admission, `UnrollTriggerManual`, `UnrollTriggerFraudSpend`). It mirrors the unroll package's `StartTrigger` so `vtxo` and `actormsg` can thread the trigger through `ForceUnroll` without importing `unroll` (which would form a cycle); the darepod chain resolver bridge converts it back at the seam.
- `ExitPolicyKind` / `ExitPolicyRef` / `ExitPolicy` — Durable exit-spend policy identity for a forced exit. `ExitPolicyKind` is a string-typed enum of the non-standard policies that ride `ForceUnroll` (`ExitPolicyVHTLCClaim`, `ExitPolicyVHTLCRefundWithoutReceiver`), with `Valid()` true only for those two vHTLC kinds; `ExitPolicyRef` is the policy-specific durable reference (e.g. a vHTLC recovery job id), kept a distinct type so `Kind` and `Ref` can't be transposed; `ExitPolicy` bundles the pair as one identity validated at the registry admission boundary. A `None` `ExitPolicy` selects the standard VTXO timeout policy.
- `RegisterIntentMsg` — Carries pre-composed cooperative intent package to
  round actor. The `TriggerRegistration bool` field controls whether the
  round FSM immediately fires `IntentRequested` after accepting the intent
  (`true` for directed sends) or parks in `PendingRoundAssembly` for
  batching (`false` for refresh/leave flows).
- `TriggerBoardMsg` — Carries VTXO amounts for boarding registration to round actor.
- `SelectedVTXO` — Describes a VTXO selected for spend (outpoint, amount, pkscript).
- `RoundActorServiceKey()` / `VTXOManagerServiceKey()` / `VTXOActorServiceKey(outpoint wire.OutPoint)` — Service key constructors for actor discovery. `VTXOActorServiceKey` encodes the target outpoint into the key so each per-VTXO actor gets a unique, deterministic key.

## Relationships

- **Depends on**: `baselib/actor` (ServiceKey, Message interfaces).
- **Depended on by**: `vtxo` (re-exports admission types as aliases), `wallet` (sends admission requests), `round` (receives `RoundReceivable` messages), `darepod` (wiring service keys).

## Invariants

- All cross-boundary actor messages must implement the appropriate marker interface (`RoundReceivable`, `VTXOManagerMsg`, etc.) for type-safe actor routing.
- Service key names are constants (`RoundActorServiceKeyName`, `VTXOManagerServiceKeyName`) shared across the codebase for consistent actor discovery.
- `SelectedVTXO` intentionally duplicates minimal VTXO info to avoid `wallet` importing `vtxo.Descriptor`.

## Deep Docs

- [lib/CLAUDE.md](../CLAUDE.md) — Parent lib package overview.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
