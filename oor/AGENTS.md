# oor

## Purpose

Client-side out-of-round (OOR) VTXO transfer coordination without waiting for
normal rounds, preserving deterministic transaction construction and crash-safe
resume semantics.

## Key Types

For field-level detail, use `go doc github.com/lightninglabs/darepo-client/oor.<Symbol>`.
State transitions and validation rules live under [Invariants](#invariants).

### Session, FSM & Actor Infrastructure

- `SessionID` — stable session identifier (Ark txid hash in v0).
- `Environment` — FSM environment exposing SessionID and external system
  access.
- `OutboxHandler` — interface executing FSM outbox requests (RPC,
  signing, persistence).
- `SignArkPSBT` — signs Ark PSBT inputs on the checkpoint 2-of-2 collab
  leaf using `MultiPrevOutFetcher` for BIP-341 sighashes across
  multi-input transfers.
- `ClientActorCfg` — configuration for `OORClientActor`. Notable fields:
  `OutboxHandler`, `ServerConn`, `PackageStore`, `DeliveryStore`,
  `VTXOManager`, `VTXOStore`, optional `LedgerSink fn.Option[ledger.Sink]`
  (fire-and-forget accounting), optional `IncomingVTXOObserver
  IncomingVTXONotifier` (callback fired after incoming VTXOs are durably
  materialized — lets daemon subsystems arm work without depending on
  `oor`), `SigningEffect` (route signing through a separate actor),
  `Limits *ReceiveLimits` (defaults via `DefaultReceiveLimits`).
- `OORClientActor` — durable actor wrapping per-session state machines.
  Handles outgoing and incoming flows via three-phase async resolution;
  emits `VTXOSentMsg` / `VTXOReceivedMsg` to ledger at the two state
  transitions it owns (`FinalizeAcceptedEvent` and
  materialized-VTXO notification).
- `SigningEffectActor` — separate durable actor performing wallet
  signing outside the OOR turn. Receives `SigningEffectRequest`
  messages (TLV type `0x7020`, kinds `signingEffectRequestArk` /
  `signingEffectRequestCheckpoint`), delegates to `SigningOutboxHandler`,
  and feeds results back via `DriveEventRequest`. Stale signing results
  for an advanced FSM are silently discarded. Registered as
  `SigningEffectActorID = "oor-signing-effect"`; lifecycle via
  `NewSigningEffectActor` / `StopAndWait`.
- `ReceiveLimits` / `DefaultReceiveLimits` — defense-in-depth bounds on
  incoming receive (`MaxCheckpoints=64`, `MaxVTXOMatches=128`,
  `MaxMailboxItems=10000`, `MaxMailboxScriptBytes=10000`). Zero fields
  are normalized; codec factories capture limits so deserialization
  enforces them.
- `emitVTXOSent` / `emitVTXOsReceived` — internal ledger emitters
  (gated on `fn.Some(LedgerSink)`).
- `NewRetryCallbackRef` — bridges timeout-actor expiry notifications
  into OOR `ResumeSessionRequest` for event-driven retry.

### Actor Messages (`OORDurableMsg` / `ActorMsg`)

- `ResolveIncomingTransferRequest` — TLV-durable (`0x7016`); persisted
  by the ingress route so phase-1 indexer resolution resumes after a
  crash.
- `DriveEventRequest` — generic wrapper: `(Event, SessionID)`. Used by
  outbox callbacks and durable unary response routes to feed events
  back into a running FSM.
- `FindOutgoingSessionByIdempotencyKeyRequest` /
  `…Response` — TLV-durable (`0x7018`); idempotent outgoing-session
  lookup. `Found=false` ⇒ no session matches yet.
- `ListSessionsRequest` / `…Response` — TLV-durable (`0x7017`).
  Carries `SessionDirection` filter and `PendingOnly`. Response is
  `[]SessionSummary`.
- `SessionSummary` — diagnostic projection (SessionID, Direction,
  Phase, Pending, RetryAfter, RetryReason, InputOutpoints,
  InputAmountSat, RecipientCount).
- `SessionDirection` — enum (`All`, `Outgoing`, `Incoming`).

### Outbox Events

- `QueryIncomingTransferRequest` — emitted after persisting
  `ReceiveResolving`; mapped to
  `serverconn.SendListOORRecipientEventsByScriptRequest`.
- `QueryIncomingMetadataRequest` — emitted after
  `IncomingTransferEvent`; mapped to
  `serverconn.SendListVTXOsByScriptsRequest`.
- `MaterializeIncomingVTXOsRequest` — sent to the wallet/state layer to
  persist incoming VTXO records (carries Ark PSBT, checkpoint PSBTs,
  recipients, resolved `MetadataMatches`).
- `SendIncomingAckRequest` — asks transport to ack the incoming
  transfer.
- `IncomingTransferNotification` — emitted alongside metadata query.
- `ScheduleRetryRequest` — retryable-outbox scheduling via the timeout
  actor.

### FSM Events & Incoming Receive States

- Events: `IncomingTransferEvent`, `IncomingMetadataResolvedEvent`,
  `IncomingHandledEvent`, `IncomingAckSentEvent`.
- `ReceiveState`: `ReceiveIdle` → `ReceiveResolving` (durable hint
  persisted, waiting for phase-1 indexer outside the actor tx) →
  `ReceiveNotified` (package received, awaiting materialization) →
  `ReceiveAwaitingAck` (materialized, awaiting transport ack) →
  `ReceiveCompleted`.

### Outbox Handler Chain & Callbacks

- `LocalPersistenceOutboxHandler` — handles `MarkInputsSpentRequest`,
  `QueryIncomingMetadataRequest`, `MaterializeIncomingVTXOsRequest`,
  `SendIncomingAckRequest`; delegates everything else to `Next`. Also
  implements `IncomingMetadataRecipientFilter` so the transport layer
  can pre-filter owned recipients.
- `SpendCompleter` — `func(ctx, []wire.OutPoint) error` routing OOR
  spend completion through the VTXO manager. `nil` ⇒ direct store
  writes (migration compat).
- `IncomingClientKeyResolver` — `func(ctx, ArkRecipientOutput)
  (keychain.KeyDescriptor, error)`. Returns
  `ErrIncomingRecipientNotOwned` for outputs belonging to other
  clients.
- `IncomingMetadataResolver` — `func(ctx, SessionID,
  ArkRecipientOutput, *psbt.Packet, []*psbt.Packet)
  (IncomingVTXOMetadata, error)`.
- `IncomingMetadataRecipientFilter` — `FilterIncomingMetadataRecipients`.
- `IncomingVTXONotifier` — `func(ctx, []*vtxo.Descriptor) error` for
  non-actor consumers (systest, etc.) after durable materialization.
- `OutboxHandlerConfig` / `NewOutboxHandler` — shared factory for the
  two-layer chain `LocalPersistenceOutboxHandler → SigningOutboxHandler`,
  used identically by production darepod and systest.

### Snapshot, Phase & Adapter Types

- `OutgoingSnapshot` (Phase, ArkPSBT, TransferInputSnapshots,
  RetryAfter, FailReason), `OutgoingPhase` (`ark_sign_requested`,
  `submit_sent`, `cosigned`, `finalize_sent`, `local_vtxo_update`,
  `completed`, `failed`).
- `IncomingSnapshot`, `IncomingPhase` (`resolve_pending`,
  `materialize_pending`, `ack_pending`, `completed`, `failed`).
- `TransferInputSnapshot` — portable encoding of client-side signing
  context required to finalize checkpoint PSBTs after restart.
- `IncomingVTXOMetadata` — lineage metadata for incoming OOR VTXOs
  (`ChainDepth` = OOR checkpoint hop count).
- `IncomingMetadataMatch` — authoritative per-output metadata for one
  materialized Ark output.
- `IncomingMetadataMatchesFromResponse` — filters a
  `ListVTXOsByScriptsResponse` down to current-session outputs.
- `IncomingTransferEventFromResponse` — validates and converts a
  `ListOORRecipientEventsByScriptResponse` payload into an
  `IncomingTransferEvent`.
- `NewResolveIncomingTransferRequest` — converts a lightweight
  `IncomingOOREvent` proto to a `ResolveIncomingTransferRequest`
  (shared by darepod / systest).
- `IncomingResolveCorrelationID` / `IncomingMetadataCorrelationID`
  (+ `Parse…`) — stable correlation IDs for phase-1 / phase-2 durable
  queries.

## Relationships

- **Depends on**: `baselib/protofsm`, `baselib/actor`, `serverconn`,
  `lib/arkscript`, `ledger` (`Sink` + emission messages), `timeout`
  (`TimeoutActor`), `lnd/input` (signer interface for
  `SigningEffectActor`).
- **Depended on by**: `darepod`.
- **Sends**:
  - → `serverconn`: `SendSubmitPackageRequest`,
    `SendFinalizePackageRequest`, `SendIncomingAckRequest`.
  - → `serverconn` (durable unary, via outbox):
    `QueryIncomingTransferRequest` →
    `SendListOORRecipientEventsByScriptRequest`;
    `QueryIncomingMetadataRequest` →
    `SendListVTXOsByScriptsRequest`.
  - → `db` (via outbox): `MarkInputsSpentRequest`.
  - → `wallet`: `MaterializeIncomingVTXOsRequest`.
  - → `vtxo` manager: `VTXOsMaterializedNotification`.
  - → `ledger` (when `LedgerSink` is `fn.Some`): `VTXOSentMsg` on
    `FinalizeAcceptedEvent`; `VTXOReceivedMsg{Source=SourceOOR}` per
    materialized descriptor.
- **Receives**:
  - ← `serverconn` (`EventRouter`): `SubmitAcceptedEvent`,
    `FinalizeAcceptedEvent`, `ResolveIncomingTransferRequest`.
  - ← `serverconn` durable unary response routes:
    `DriveEventRequest{IncomingTransferEvent}`,
    `DriveEventRequest{IncomingMetadataResolvedEvent}`.
  - ← local persistence callback path:
    `DriveEventRequest{IncomingHandledEvent}`.
  - ← API: `StartTransferRequest`, `DriveEventRequest`,
    `RestoreSessionRequest`, `ResumeSessionRequest`,
    `ListSessionsRequest`.

## Multi-Tree Ancestry + Lineage Cap

- `IncomingVTXOMetadata.Ancestry []vtxo.Ancestry` replaces the
  singular `TreePath`. The durable mailbox TLV record is
  `incomingMetadataMatchAncestryPathsRecordType`; per-entry layout is
  `(TreePath, CommitmentTxID, InputIndices, TreeDepth)`.
- Server-side over-cap submit rejection surfaces as
  `*oorpb.SubmitRejectedError{Code: OOR_REJECT_LINEAGE_TOO_LARGE}`;
  `ClassifySubmitError` maps it to `*ErrLineageTooLarge` so wallet
  callers can switch on the cause without depending on the proto type.

## Invariants

- Checkpoint output collab path is 2-of-2
  `MultiSigCollabTapLeaf(clientKey, operatorKey)`, not single-sig.
- `signCustomCheckpointPSBT` re-verifies that the custom spend path
  binds to the VTXO pkScript via `SpendPath.VerifyBindsToPkScript`
  before signing — covers persisted `TransferInputSnapshot`s resumed
  from disk that bypassed `BuildCustomTransferInputs`.
- Condition witness encoding is bounded by `maxConditionWitnessItems =
  64` and `maxConditionWitnessItemBytes = 520` (matches Bitcoin's
  `MAX_SCRIPT_ELEMENT_SIZE`). Both encode/decode enforce this via
  `wire.ReadVarBytes` so a crafted blob cannot cause large
  allocations. Policy template decoding uses the separate
  `arkscript.readVarBytes` capped at `MaxPolicyTemplateBytes` (64 KiB).
- Submit-time only does structural validation
  (`ValidateSubmitPackage`); full script VM validation runs at
  finalize (requires both signatures).
- Incoming ancestor packages have a per-ancestor checkpoint count cap
  (`packageArtifactsFromRPC`, `maxAncestorPackages = 64`) to prevent
  resource exhaustion from a misbehaving indexer.
- Indexer-supplied `tree_depth` is cross-checked against the
  reconstructed path via `arkrpc.ValidateAncestryPathDepth` in
  `ancestryFromRPC`. Truncated depth or under-reported CSV window
  rejects the package.
- `validateIncomingPackageGraph` runs from
  `IncomingTransferEventFromResponseWithLimits` after assembly as a
  final defense-in-depth check before FSM dispatch.
- Point-of-no-return: server co-signing the checkpoint
  transaction(s). After that, client must resume with byte-identical
  co-signed PSBTs (deterministic construction).
- Transport outbox events (submit / finalize / ack) are durably
  enqueued in the actor transition; transport side effects run
  outside the actor DB tx and are retried via the actor delivery
  store.
- Outgoing finalize ordering: local input-spend completion is driven
  **before** the package write so the VTXO manager joins the durable
  OOR actor tx instead of racing a second SQLite writer.
- Incoming receive never performs synchronous unary RPCs inside the
  durable actor DB tx. Both phase-1 hint resolution and phase-2
  authoritative metadata lookup are durable `serverconn` query
  messages, delivered back as fresh durable events.
- `LocalPersistenceOutboxHandler.CallbackRef` (on the inner
  `SigningOutboxHandler`) receives async materialization results so
  indexer queries run outside the actor tx, preventing SQLite
  write-lock starvation.
- `handleMarkInputsSpent` skips non-local outpoints, routes the rest
  to `CompleteSpend` (or direct store writes if `nil`).
  `actor.ErrNoActorsAvailable` returns a retryable error.
- `handleMaterializeIncoming` only calls `NotifyIncomingVTXOs`
  directly when `hasActorDBTx` is false; inside a durable actor tx,
  notification is deferred to `notifyMaterializedVTXOs` via the
  `IncomingHandledEvent` follow-up path so the manager sees
  materialization exactly once.
- `ListSessionsRequest` sorts results deterministically by SessionID
  string; direction / pending filters apply after projection.
- `oorCheckpointVersion = 2`. Restore accepts versions 1 and 2.
- Self-transfer: a `ResolveIncomingTransferRequest` for a session
  with an active outgoing session errors until the outgoing session
  terminates; then the outgoing entry is deleted and an incoming
  session is created in its place.
- `SigningEffectActor` requests are durable: OOR persists FSM state
  and enqueues the request before signing runs. A restart-duplicate
  that reaches OOR after the FSM has advanced is silently discarded
  by `DriveEventRequest`.
- `ReceiveLimits` are propagated through codec factories
  (`newOORActorCodec`, `NewSigningEffectCodec`) so every deserialized
  message enforces the same caps as the in-memory path. Codec
  instances are shared per actor.
- `StartTransferRequest.IdempotencyKey`: when non-empty, the actor
  checks `FindOutgoingSessionByIdempotencyKeyRequest` before creating
  a new session, returning `StartTransferResponse{Existing: true}` on
  hit. Empty key preserves the historical deterministic
  (Ark txid) session.

## Deep Docs

- [oor/doc.go](doc.go) — Package overview.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
</content>
</invoke>
