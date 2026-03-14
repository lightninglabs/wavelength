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
- `ClientActorCfg` — Configuration for OORClientActor (OutboxHandler, ServerConn, PackageStore, DeliveryStore, VTXOManager).
- `IncomingVTXOMetadata` — Lineage metadata for incoming OOR VTXOs including `ChainDepth` (OOR checkpoint hop count).
- `OORClientActor` — Durable actor wrapping per-session state machines.

## Relationships

- **Depends on**: `baselib/protofsm` (FSM engine), `baselib/actor` (durable actors), `serverconn` (durable transport).
- **Depended on by**: `darepod` (wiring).
- **Sends**:
  - → `serverconn`: `SendSubmitPackageRequest`, `SendFinalizePackageRequest`, `SendIncomingAckRequest`
  - → `db` (via outbox): `MarkInputsSpentRequest`
  - → `wallet`: `MaterializeIncomingVTXOsRequest`
  - → `vtxo` manager: `VTXOsMaterializedNotification` (after incoming VTXOs are durably materialized)
- **Receives**:
  - ← `serverconn` (via EventRouter): `SubmitAcceptedEvent`, `FinalizeAcceptedEvent`, `IncomingTransferEvent`
  - ← API: `StartTransferRequest`, `DriveEventRequest`, `RestoreSessionRequest`, `ResumeSessionRequest`

## Invariants

- Checkpoint output collab path is 2-of-2 `MultiSigCollabTapLeaf(clientKey, operatorKey)`, not single-sig. Both parties must sign the Ark tx that spends checkpoint outputs.
- At submit time only structural validation runs (`ValidateSubmitPackage`); full script VM validation requires both signatures and runs at finalize.
- Point-of-no-return: when server co-signs checkpoint transaction(s).
- After checkpoint signature, client must resume and obtain byte-identical co-signed PSBTs (deterministic construction).
- Transport outbox events (submit, finalize, ack) are Tell'd to ServerConn within the actor's DB transaction for atomic enqueue (same-DB tx joining via `ExecTx`).
- Package persistence tracks finalized outgoing packages and local input bindings for recovery.

## Deep Docs

- [oor/doc.go](doc.go) — Package overview.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
