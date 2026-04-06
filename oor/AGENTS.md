# oor

## Purpose

Server-side out-of-round (OOR) transfer coordinator FSM. Manages direct VTXO
transfers between clients outside of round periods, handling input locking,
co-signing, finalization, and recipient notification.

## Key Types

- `TransferCoordinatorActor` (alias `Actor`) — Durable OOR transfer coordinator
  with FSM state persistence and `ClientsConn` push for response delivery.
  Implements `ActorBehavior[OORDurableMsg, ActorResp]` directly (no intermediate
  behavior wrapper). Each `ActorMsg` type implements `TLVMessage` directly
  (TLVType, Encode, Decode), so the durable mailbox serializes messages without
  an intermediate envelope layer.
- `OORDurableMsg` — Message constraint for the durable actor mailbox; embeds
  `actor.TLVMessage` so both application messages and framework restart messages
  satisfy it.
- `SessionID` — OOR transfer session identifier (derived from ArkTxid).
- `State` — Sealed interface for FSM states (Idle through Finalized/Failed).
- `Event` — Inbound events (SubmitRequest, FinalizeRequest, etc.).
- `OutboxEvent` — Outbound side effects (notify recipients, persist state).
- `SubmitOORRequest` / `FinalizeOORRequest` — Primary actor messages implementing
  `TLVMessage` directly (dispatched via `AddEnvelopeRoute` from `server_oor.go`).
- `SubmitOORResponse` / `FinalizeOORResponse` — Response types implementing
  `clientconn.ClientMessage` for push delivery via `ClientsConn.Tell()`.
- `InProcessOutboxDriver` — Reusable outbox handler for the OOR FSM session
  lifecycle (lock, validate, co-sign, finalize, notify).
- `RecipientNotifier` — Interface for best-effort recipient notification after
  durable event persistence; implemented by the indexer layer.
- `RecipientEventStore` — Persists per-recipient notification cursors and payloads.
- `VTXOSigningDescriptor` — Per-input signing metadata (outpoint, owner key,
  exit delay) threaded through the FSM for checkpoint construction.

## Relationships

- **Depends on**: `clientconn` (response push via `ClientsConn`), `db` (OOR
  session persistence), `vtxo` (VTXO locking during transfers).
- **Depended on by**: root `darepo` (wiring in `server_oor.go`), `indexer`
  (OOR event queries, `RecipientNotifier` implementation).
- **Messages to/from**:
  - Receives submit/finalize requests <- `clientconn` via `AddEnvelopeRoute`
    (fire-and-forget Tell from clients; ClientID extracted from `env.Sender`).
  - Pushes `SubmitOORResponse`/`FinalizeOORResponse` -> originating client via
    `ClientsConn.Tell()` (wrapped in `SendServerEventRequest`).
  - Calls `RecipientNotifier.NotifyRecipientEvent()` -> indexer layer for
    best-effort recipient push after finalization.
  - Reads/writes OOR session state -> `db`.

## Invariants

- VTXO inputs must be locked before validation proceeds (prevents double-spend).
- Co-signing happens atomically: either all inputs are co-signed or none.
- Ark PSBTs are co-signed before persisting OOR packages (ordering fix: sign
  then persist, not persist then sign).
- Recipients are notified only after finalization is persisted.
- Failed transfers must release all VTXO locks and clean up the session map
  entry to prevent leaks.
- `FinalCheckpointPSBTs` are threaded through FSM states so they survive
  restart and are available for re-notification of `AwaitingRecipientsNotify`
  sessions.
- Self-contained VTXO spend metadata (outpoint, owner key, exit delay) is
  persisted alongside OOR packages for checkpoint construction.
- OOR transfer outcomes are instrumented via metrics actor events
  (`OORTransferStartedMsg`/`OORTransferCompletedMsg`).
- Structured logging emits at every key lifecycle event (submit, co-sign,
  finalize, restore, lock/unlock, validation, notification).

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
