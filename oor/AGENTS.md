# oor

## Purpose

Server-side out-of-round (OOR) transfer coordinator FSM. Manages direct VTXO
transfers between clients outside of round periods, handling input locking,
co-signing, finalization, and recipient notification.

## Key Types

- `Actor` — Durable OOR transfer coordinator with FSM state persistence.
- `SessionID` — OOR transfer session identifier.
- `State` — Sealed interface for FSM states (Idle through Finalized/Failed).
- `Event` — Inbound events (SubmitRequest, FinalizeRequest, etc.).
- `OutboxEvent` — Outbound side effects (notify recipients, persist state).
- `SubmitOORRequest` / `FinalizeOORRequest` — Primary actor messages.

## Relationships

- **Depends on**: `clientconn` (outbound events to clients), `db` (OOR session persistence), `vtxo` (VTXO locking during transfers).
- **Depended on by**: root `darepo` (wiring), `indexer` (OOR event queries).
- **Messages to/from**:
  - Receives submit/finalize requests <- `clientconn` (from clients).
  - Sends recipient notifications -> `clientconn` (to clients).
  - Reads/writes OOR session state -> `db`.

## Invariants

- VTXO inputs must be locked before validation proceeds (prevents double-spend).
- Co-signing happens atomically: either all inputs are co-signed or none.
- Recipients are notified only after finalization is persisted.
- Failed transfers must release all VTXO locks.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
