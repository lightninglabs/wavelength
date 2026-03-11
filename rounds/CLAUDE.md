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
- `JoinRoundRequestFromProto`, `NoncesFromProto`, `PartialSigsFromProto`, etc. — Exported proto→domain conversion helpers in `proto_convert.go`, called from `server_rounds.go` `AddEnvelopeRoute` Adapt closures.

## Relationships

- **Depends on**: `batch` (tx building, MuSig2 coordination), `batchwatcher` (confirmation monitoring), `clientconn` (outbound events to clients), `vtxo` (VTXO locking during rounds).
- **Depended on by**: `indexer` (round event subscription), `lndbackend` (chain queries), root `darepo` (wiring).
- **Messages to/from**:
  - Receives JoinRoundRequest, nonces, partial sigs <- `clientconn` via `AddEnvelopeRoute` (fire-and-forget Tell from clients).
  - Sends round events, commitment tx, aggregated nonces -> `clientconn` (to clients via bridge egress).
  - Sends batch build requests -> `batch`.
  - Receives confirmation events <- `batchwatcher`.
  - Proto→domain conversion helpers exported in `proto_convert.go` for use by server wiring layer (`server_rounds.go`).

## Invariants

- Tree signatures must be validated BEFORE input signatures are released.
- VTXO locks must be acquired before batch building and released on failure.
- Round state is checkpointed atomically; crash before checkpoint means no partial state persists.
- Boarding input signatures are only broadcast after all forfeit signatures are collected.

## Deep Docs

- [rounds/README.md](README.md) — Full state machine walkthrough with diagrams.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
