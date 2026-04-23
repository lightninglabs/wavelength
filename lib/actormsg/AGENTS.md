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
- `ForceUnrollRequest` / `ForceUnrollResponse` — Ask-message that routes an operator or chain-resolver unroll trigger through the VTXO manager into the per-VTXO FSM. `ForceUnrollResponse.Accepted` is true when the request caused a state transition; when false, `Reason` distinguishes `"no such vtxo"` from `"already terminal"` so callers don't misread a silent self-loop as success.
- `RegisterIntentMsg` — Carries pre-composed cooperative intent package to round actor.
- `TriggerBoardMsg` — Carries VTXO amounts for boarding registration to round actor.
- `SelectedVTXO` — Describes a VTXO selected for spend (outpoint, amount, pkscript).
- `RoundActorServiceKey()` / `VTXOManagerServiceKey()` — Service key constructors for actor discovery.

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
