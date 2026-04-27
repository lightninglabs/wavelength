# oor

## Purpose

Client-side out-of-round (OOR) VTXO transfer coordination without waiting for
normal rounds, preserving deterministic transaction construction and crash-safe
resume semantics.

## Key Types

### Session Identifiers and FSM Infrastructure

- `SessionID` — Stable session identifier (Ark txid hash in v0).
- `Environment` — FSM environment providing SessionID and external system access.
- `OutboxHandler` — Interface for executing FSM outbox requests (RPC, signing, persistence).
- `SignArkPSBT` — Signs Ark PSBT inputs using the client key on the checkpoint 2-of-2 collab leaf; uses `MultiPrevOutFetcher` for correct BIP-341 sighash across multiple inputs.
- `ClientActorCfg` — Configuration for OORClientActor (OutboxHandler,
  ServerConn, PackageStore, DeliveryStore, VTXOManager, and optional `LedgerSink fn.Option[ledger.Sink]` for fire-and-forget accounting emission).
- `OORClientActor` — Durable actor wrapping per-session state machines. Handles both outgoing transfers and incoming receive via three-phase async resolution. Emits `VTXOSentMsg` / `VTXOReceivedMsg` to the ledger actor at the two points the package observes state transitions it owns (FinalizeAcceptedEvent and materialized-VTXO notification).
- `emitVTXOSent(ctx, sessionID, inputs)` — Internal helper called on `FinalizeAcceptedEvent` after the outgoing package has been persisted. Sums `TransferInputs` to get the total sent amount and Tells a `VTXOSentMsg` with the 32-byte session ID stamped on the entry. Gated on `fn.Some(ledgerSink)` — `fn.None` tests see a silent no-op.
- `emitVTXOsReceived(ctx, descriptors)` — Internal helper called in `notifyMaterializedVTXOs`. Tells one `VTXOReceivedMsg` per descriptor with `Source = SourceOOR` so the entry books as `transfers_in` on the ledger side.
- `NewRetryCallbackRef` — Bridges timeout actor expiry notifications into OOR actor `ResumeSessionRequest` messages for event-driven retry scheduling.
- `IncomingSnapshot` / `NewIncomingSnapshot` — Serializable snapshot of incoming receive session state for diagnostics.

### Actor Messages (OORDurableMsg / ActorMsg)

- `ResolveIncomingTransferRequest` — TLV-durable actor message (TLV type
  `0x7016`) persisted by the ingress route. Carries SessionID,
  RecipientPkScript, and RecipientEventID so the actor can resume
  phase-1 indexer resolution after a crash.
- `DriveEventRequest` — Generic actor message that wraps an Event and a
  SessionID; used to feed FSM events back into a running session from
  outbox callbacks and durable unary response routes.

### Outbox Events (OutboxEvent)

- `QueryIncomingTransferRequest` — Outbox event emitted after persisting
  `ReceiveResolving`; actor.go maps this to a
  `serverconn.SendListOORRecipientEventsByScriptRequest` durable query.
- `QueryIncomingMetadataRequest` — Outbox event emitted after
  `IncomingTransferEvent` is processed; actor.go maps this to a
  `serverconn.SendListVTXOsByScriptsRequest` durable query.
- `MaterializeIncomingVTXOsRequest` — Outbox event carrying the Ark PSBT,
  checkpoint PSBTs, recipients, and resolved `MetadataMatches`; sent to
  the wallet/state layer to persist incoming VTXO records.
- `SendIncomingAckRequest` — Outbox event that asks the transport layer to
  ack the incoming transfer to the server.
- `IncomingTransferNotification` — Outbox event emitted alongside metadata query during incoming transfer processing.
- `ScheduleRetryRequest` — Outbox event for scheduling retryable outbox operations via the timeout actor.

### Events (Event / ReceiveState)

- `IncomingTransferEvent` — FSM event carrying the full Ark PSBT and
  checkpoint PSBTs for an incoming transfer; delivered by the phase-1
  durable unary response route.
- `IncomingMetadataResolvedEvent` — FSM event delivering authoritative
  metadata query results back into the receive FSM; delivered by the
  phase-2 durable unary response route.
- `IncomingHandledEvent` — FSM event indicating the wallet layer has
  persisted incoming VTXOs; carries `MaterializedOutpoints` for the
  durable callback round-trip.
- `IncomingAckSentEvent` — FSM event driving `ReceiveAwaitingAck → ReceiveCompleted` transition.

### Incoming Receive FSM States (ReceiveState)

- `ReceiveIdle` — Initial state; no pending incoming transfer.
- `ReceiveResolving` — Durable hint persisted; waiting for the phase-1
  indexer query (ListOORRecipientEventsByScript) to return the full Ark
  package outside the actor transaction.
- `ReceiveNotified` — Full Ark/checkpoint package received; waiting for
  local materialization to complete.
- `ReceiveAwaitingAck` — VTXOs materialized; waiting for ack transport to
  complete.
- `ReceiveCompleted` — Terminal success state.

### Shared Adapters and Metadata Types

- `TransferInputSnapshot.RequiredSequence` / `TransferInputSnapshot.RequiredLockTime`
  — Persisted nSequence and nLockTime values required by a custom spend path
  (e.g. vHTLC refunds using CLTV-gated leaves). Both survive TLV snapshot
  round-trips so resumed custom OOR spends rebuild byte-identical checkpoint
  and Ark transactions. Encoded as TLV types 15 and 16 respectively; omitted
  (zero) for standard spend paths.
- `IncomingVTXOMetadata` — Lineage metadata for incoming OOR VTXOs including `ChainDepth` (OOR checkpoint hop count).
- `IncomingMetadataMatch` — Authoritative metadata for one materialized
  incoming Ark output, keyed by OutputIndex.
- `IncomingMetadataMatchesFromResponse` — Filters a
  `ListVTXOsByScriptsResponse` down to outputs matching the current Ark
  session and converts them to `[]IncomingMetadataMatch`.
- `IncomingTransferEventFromResponse` — Validates and converts one
  `ListOORRecipientEventsByScriptResponse` payload into an
  `IncomingTransferEvent` for the receive FSM.
- `NewResolveIncomingTransferRequest` — Converts a lightweight
  `IncomingOOREvent` notification proto into a
  `ResolveIncomingTransferRequest`; shared by darepod and systest.
- `IncomingResolveCorrelationID` / `ParseIncomingResolveCorrelationID` —
  Stable correlation ID helpers for phase-1 durable queries.
- `IncomingMetadataCorrelationID` / `ParseIncomingMetadataCorrelationID` —
  Stable correlation ID helpers for phase-2 durable queries.
- `NewOutboxHandler` / `OutboxHandlerConfig` — Shared factory for the standard two-layer outbox handler chain (LocalPersistenceOutboxHandler → SigningOutboxHandler).

## Relationships

- **Depends on**: `baselib/protofsm` (FSM engine), `baselib/actor` (durable actors), `serverconn` (durable transport), `lib/arkscript` (policy-backed tapscript for checkpoint signing and VTXO policy templates in transfer TLV records), `ledger` (`Sink` + emission message types).
- **Depended on by**: `darepod` (wiring).
- **Sends**:
  - → `serverconn`: `SendSubmitPackageRequest`, `SendFinalizePackageRequest`, `SendIncomingAckRequest`
  - → `serverconn` (durable unary, via outbox): `QueryIncomingTransferRequest` → `SendListOORRecipientEventsByScriptRequest`; `QueryIncomingMetadataRequest` → `SendListVTXOsByScriptsRequest`
  - → `db` (via outbox): `MarkInputsSpentRequest`
  - → `wallet`: `MaterializeIncomingVTXOsRequest`
  - → `vtxo` manager: `VTXOsMaterializedNotification` (after incoming VTXOs are durably materialized)
  - → `ledger` actor (via `ledger.Sink` Tell, when `fn.Some`): `VTXOSentMsg` on FinalizeAcceptedEvent (post-persistence); `VTXOReceivedMsg` with `Source=SourceOOR` per materialized descriptor
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
- `signCustomCheckpointPSBT` re-verifies that the custom spend path binds to the VTXO pkScript via `SpendPath.VerifyBindsToPkScript` before signing. This defense-in-depth check covers persisted `TransferInputSnapshot`s resumed from disk that bypassed the `BuildCustomTransferInputs` constructor.
- Condition witness encoding/decoding (`encodeConditionWitness`/`decodeConditionWitness`) is bounded by `maxConditionWitnessItems = 64` items and `maxConditionWitnessItemBytes = 520` bytes per item (matching Bitcoin's `MAX_SCRIPT_ELEMENT_SIZE`). Both functions enforce these limits via `wire.ReadVarBytes` so a crafted or corrupted durable blob cannot cause large memory allocations. (The separate `arkscript.readVarBytes` used by policy template decoding caps at `MaxPolicyTemplateBytes` (64 KiB); the 520-byte cap applies only to persisted OOR condition witnesses.)
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
