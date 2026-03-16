# oor

## Purpose

Client-side out-of-round (OOR) VTXO transfer coordination without waiting for
normal rounds, preserving deterministic transaction construction and crash-safe
resume semantics.

## Key Types

- `SessionID` — Stable session identifier (Ark txid hash in v0).
- `Environment` — FSM environment providing SessionID and external system access.
- `OutboxHandler` — Interface for executing FSM outbox requests (RPC, signing, persistence).
- `SignArkPSBT` — Signs Ark PSBT inputs using the client key on the checkpoint 2-of-2 collab leaf; uses `MultiPrevOutFetcher` for correct BIP-341 sighash across multiple inputs.
- `ClientActorCfg` — Configuration for OORClientActor (OutboxHandler,
  ServerConn, PackageStore, DeliveryStore, VTXOManager).
- `IncomingVTXOMetadata` — Lineage metadata for incoming OOR VTXOs including `ChainDepth` (OOR checkpoint hop count).
- `OORClientActor` — Durable actor wrapping per-session state machines. Handles both outgoing transfers and incoming receive via three-phase async resolution.
- `ResolveIncomingTransferRequest` — TLV-durable message persisted by the
  ingress route, resumed via a durable unary indexer query.
- `QueryIncomingTransferRequest` / `QueryIncomingMetadataRequest` — durable
  transport outbox events that `oor/actor.go` maps to transport-native
  durable `serverconn` query messages after commit.
- `serverconn.SendListOORRecipientEventsByScriptRequest` /
  `serverconn.SendListVTXOsByScriptsRequest` — durable post-commit transport
  messages used to resolve incoming hints and authoritative metadata.
- `AdaptIncomingOOREvent` / `NewResolveIncomingTransferRequest` — Shared adapters for the notification→query pattern used by both darepod and systest.
- `NewOutboxHandler` / `OutboxHandlerConfig` — Shared factory for the standard two-layer outbox handler chain (LocalPersistenceOutboxHandler → SigningOutboxHandler).
- `ReceiveResolving` — FSM state indicating a durable hint is persisted and
  pending the post-commit unary indexer resolution.

## Relationships

- **Depends on**: `baselib/protofsm` (FSM engine), `baselib/actor` (durable actors), `serverconn` (durable transport).
- **Depended on by**: `darepod` (wiring).
- **Sends**:
  - → `serverconn`: `SendSubmitPackageRequest`, `SendFinalizePackageRequest`, `SendIncomingAckRequest`
  - → `db` (via outbox): `MarkInputsSpentRequest`
  - → `wallet`: `MaterializeIncomingVTXOsRequest`
  - → `vtxo` manager: `VTXOsMaterializedNotification` (after incoming VTXOs are durably materialized)
- **Receives**:
  - ← `serverconn` (via EventRouter): `SubmitAcceptedEvent`, `FinalizeAcceptedEvent`, `ResolveIncomingTransferRequest`
  - ← `serverconn` durable unary response routes:
    `DriveEventRequest{IncomingTransferEvent}`,
    `DriveEventRequest{IncomingMetadataResolvedEvent}`
  - ← local persistence callback path:
    `DriveEventRequest{IncomingHandledEvent}`
  - ← API: `StartTransferRequest`, `DriveEventRequest`, `RestoreSessionRequest`, `ResumeSessionRequest`

## Invariants

- Checkpoint output collab path is 2-of-2 `MultiSigCollabTapLeaf(clientKey, operatorKey)`, not single-sig. Both parties must sign the Ark tx that spends checkpoint outputs.
- At submit time only structural validation runs (`ValidateSubmitPackage`); full script VM validation requires both signatures and runs at finalize.
- Point-of-no-return: when server co-signs checkpoint transaction(s).
- After checkpoint signature, client must resume and obtain byte-identical co-signed PSBTs (deterministic construction).
- Transport outbox events (submit, finalize, ack) are Tell'd to ServerConn within the actor's DB transaction for atomic enqueue (same-DB tx joining via `ExecTx`).
- Package persistence tracks finalized outgoing packages and local input bindings for recovery.
- Incoming receive never performs synchronous unary RPCs inside the durable
  actor DB transaction. Both incoming-hint resolution and authoritative
  metadata lookup are emitted as transport-native durable `serverconn`
  query messages and delivered back as fresh durable messages.
- `LocalPersistenceOutboxHandler.CallbackRef` receives async materialization results so indexer queries run outside the actor tx, preventing SQLite write-lock starvation.

## Deep Docs

- [oor/doc.go](doc.go) — Package overview.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
