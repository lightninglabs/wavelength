# oor

## Purpose

Server-side out-of-round (OOR) transfer coordinator FSM. Manages direct VTXO
transfers between clients outside of round periods, handling input locking,
co-signing, finalization, and recipient notification.

## Key Types

- `TransferCoordinatorActor` (alias `Actor`) — Durable OOR transfer coordinator
  with FSM state persistence and `ClientsConn` push for response delivery.
- `OORDurableMsg` — Message constraint for the durable actor mailbox; embeds
  `actor.TLVMessage` so both application messages and framework restart messages
  satisfy it.
- `SessionID` — OOR transfer session identifier (derived from ArkTxid).
- `State` — Sealed interface for FSM states (Idle through Finalized/Failed).
- `Event` — Inbound events (SubmitRequest, FinalizeRequest, etc.).
- `OutboxEvent` — Outbound side effects (notify recipients, persist state).
- `SubmitOORRequest` / `FinalizeOORRequest` — Primary actor messages implementing
  `TLVMessage` directly (dispatched via `AddRoute` from `server_oor.go`).
- `SubmitOORResponse` / `FinalizeOORResponse` — Response types implementing
  `clientconn.ClientMessage` for push delivery via `ClientsConn.Tell()`.
- `InProcessOutboxDriver` — Reusable outbox handler for the OOR FSM session
  lifecycle (lock, validate, co-sign, finalize, notify).
- `RecipientNotifier` — Interface for best-effort recipient notification after
  durable event persistence; implemented by the indexer layer.
- `RecipientEventStore` — Persists per-recipient notification cursors and payloads.

## Relationships

- **Depends on**: `clientconn` (response push via `ClientsConn`), `db` (OOR
  session persistence), `vtxo` (VTXO locking during transfers).
- **Depended on by**: root `darepo` (wiring in `server_oor.go`), `indexer`
  (OOR event queries, `RecipientNotifier` implementation).
- **Messages to/from**:
  - Receives submit/finalize requests <- `clientconn` via `AddRoute`
    (fire-and-forget Tell from clients).
  - Pushes `SubmitOORResponse`/`FinalizeOORResponse` -> originating client via
    `ClientsConn.Tell()` (wrapped in `SendServerEventRequest`).
  - Calls `RecipientNotifier.NotifyRecipientEvent()` -> indexer layer for
    best-effort recipient push after finalization.
  - Reads/writes OOR session state -> `db`.

## Invariants

- VTXO inputs must be locked before validation proceeds (prevents double-spend).
- Co-signing happens atomically: either all inputs are co-signed or none.
- Recipients are notified only after finalization is persisted.
- Failed transfers must release all VTXO locks.
- Structured logging emits at every key lifecycle event (submit, co-sign,
  finalize, restore, lock/unlock, validation, notification).

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
