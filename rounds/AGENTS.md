# rounds

## Purpose

Server-side Ark round lifecycle FSM coordinating client registration, batch
transaction building, MuSig2 signing ceremonies, finalization, and on-chain
confirmation monitoring.

## Key Types

- `Actor` — Durable round FSM driver, processes messages and persists state.
- `RoundID` — UUID-based round identifier.
- `State` — Sealed interface for all FSM states (Created through Confirmed/Failed).
- `Event` — Inbound events triggering state transitions (ClientJoinRequest, BuildBatchTx, etc.).
- `OutboxEvent` — Outbound side effects (ClientSuccessResp, BuildBatchReq, etc.).
- `ActorMsg` — Messages sent to the round actor (JoinRoundRequest, nonces, sigs).

## Relationships

- **Depends on**: `batch` (tx building, MuSig2 coordination), `batchwatcher` (confirmation monitoring), `clientconn` (outbound events to clients), `vtxo` (VTXO locking during rounds).
- **Depended on by**: `indexer` (round event subscription), `lndbackend` (chain queries), root `darepo` (wiring).
- **Messages to/from**:
  - Receives JoinRoundRequest, nonces, partial sigs <- `clientconn` (from clients).
  - Sends round events, commitment tx, aggregated nonces -> `clientconn` (to clients).
  - Sends batch build requests -> `batch`.
  - Receives confirmation events <- `batchwatcher`.

## Invariants

- Tree signatures must be validated BEFORE input signatures are released.
- VTXO locks must be acquired before batch building and released on failure.
- Round state is checkpointed atomically; crash before checkpoint means no partial state persists.
- Boarding input signatures are only broadcast after all forfeit signatures are collected.

## Deep Docs

- [rounds/README.md](README.md) — Full state machine walkthrough with diagrams.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
