# oor

## Purpose

Client-side out-of-round (OOR) VTXO transfer coordination without waiting for
normal rounds, preserving deterministic transaction construction and crash-safe
resume semantics.

## Key Types

For field-level detail, use `go doc github.com/lightninglabs/darepo-client/oor.<Symbol>`.
State transitions and validation rules live under [Invariants](#invariants).

### Per-Session Actor Model (current)

The daemon now runs **one durable actor per OOR session** instead of one global
actor. See [docs/oor_subsystem.md](../docs/oor_subsystem.md) for the full design.

- `OORSessionActor` / `sessionBehavior` — one durable actor per session on the
  Read/Commit execution path. The FSM emits outbox events as before, but the
  actor handles them itself in one shared `driveOutbox` switch (sign inline,
  enqueue cross-actor transport to serverconn, materialize incoming VTXOs,
  schedule retries) rather than routing them through an `OutboxHandler`.
- `OORRegistryActor` — thin coordinator registered under the OOR service key,
  with a **durable inbound mailbox** (Read/Stage/Commit path, mailbox id
  `oor-client`): a server-push event is persisted before the ingress loop acks
  the operator envelope, so a crash between ingress and the per-session child
  replays the registry's idempotent spawn+forward instead of losing the event.
  It routes each message to the right session's child (hot-path `DriveEvent` via
  Tell; `StartTransfer`/`GetState` via promise handoff — the registry detaches
  the caller's promise with `actor.DetachAskPromise` and the child's result
  settles it through `OnComplete`, so the registry goroutine never parks on a
  child's signing turn and concurrent admissions sign in parallel), dedups
  outgoing transfers by idempotency key, lazily spawns children,
  routes retry-timer `ResumeSessionRequest` expiries to the owning child
  (unknown/terminal sessions are benign no-ops), reaps children on
  `SessionTerminalNotification`, and `RestoreNonTerminal` respawns **and
  resumes** in-flight sessions on boot: the restore runs as a registry message
  (`RestoreNonTerminalRequest`) on the registry goroutine, and each restored
  child is told a `ResumeSessionRequest` so it re-drives the outbox implied by
  its restored state (retry timers are in-memory and do not survive restarts).
- `ActorIDForSession` / `SessionServiceKey` / `SessionRegistryStore` —
  deterministic per-session mailbox id, the per-session receptionist key the
  registry registers each live child under (the ingress fast path resolves it
  to tell `DriveEventRequest`s straight into the child's durable mailbox,
  falling back to the registry on a miss), and the control-plane store. A
  session's full durable state lives in one `oor_session_registry` row
  (queryable columns + an opaque resume snapshot); OOR does not use the
  generic `fsm_checkpoints` blob.
- `SessionActorConfig` / `OORRegistryConfig` — per-session and coordinator
  configuration. `IncomingHandler` reuses `LocalPersistenceOutboxHandler` so the
  materialization resolvers are not reimplemented; `Signer input.Signer` signs
  Ark and checkpoint PSBTs inline during the turn; optional `LedgerSink
  fn.Option[ledger.Sink]` resolves the durable ledger actor (Tells issued inside
  Commit join the turn tx); optional `IncomingVTXOObserver IncomingVTXONotifier`
  fires after incoming VTXOs are durably materialized (lets daemon subsystems arm
  work without depending on `oor`); `Limits ReceiveLimits` bounds incoming
  receive payloads.

The legacy single global actor and the separate signing-effect actor have been
deleted; all per-session state and all wallet signing now live on the
per-session durable actor's turn.

### Session, FSM & Actor Infrastructure

- `SessionID` — stable session identifier (Ark txid hash in v0).
- `Environment` — FSM environment exposing SessionID and external system
  access.
- `OutboxHandler` — interface executing local-persistence outbox requests
  (incoming metadata query filtering, VTXO materialization). Wired as the
  `IncomingHandler` on both the session actor and the registry; its writes
  join the turn transaction via the request context. Every other outbox
  event is handled inline by the session actor's own `driveOutbox` switch.
- `SignArkPSBT` — signs Ark PSBT inputs on the checkpoint 2-of-2 collab
  leaf using `MultiPrevOutFetcher` for BIP-341 sighashes across
  multi-input transfers. Signing runs inline on the session actor's turn;
  there is no separate signing actor.
- `ReceiveLimits` / `DefaultReceiveLimits` — defense-in-depth bounds on
  incoming receive (`MaxCheckpoints=64`, `MaxVTXOMatches=128`,
  `MaxMailboxItems=10000`, `MaxMailboxScriptBytes=10000`). Zero fields
  are normalized; the `newOORActorCodec` factory captures limits so
  deserialization enforces them.
- `queueVTXOSent` / `queueVTXOsReceived` — internal ledger emitters
  (gated on `fn.Some(LedgerSink)`). Staged into `pendingLedger` during
  dispatch; `commitAck` Tells them to the durable ledger actor inside
  the commit transaction (the ledger's `DurableMailbox.Send` joins the
  ambient tx), so a committed turn can never lose its accounting. The
  VTXO-manager and fraud-observer notifications stay post-commit
  best-effort because both re-derive from the persisted VTXO rows at
  boot.
- `NewRetryCallbackRef` — bridges timeout-actor expiry notifications
  into OOR `ResumeSessionRequest` for event-driven retry.

### Actor Messages (`OORDurableMsg` / `ActorMsg`)

- `ResolveIncomingTransferRequest` — TLV-durable (`0x7016`); persisted
  by the ingress route so phase-1 indexer resolution resumes after a
  crash.
- `DriveEventRequest` — generic wrapper: `(Event, SessionID)`. Used by
  outbox callbacks and durable unary response routes to feed events
  back into a running FSM.
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
  `ReceiveCompleted`. `ReceiveResolving` arms a give-up timer
  alongside its phase-1 query (`ResolveAttempts`, persisted): the
  phase-1 query has no failure response on operator silence, so each
  timer expiry (a `ResumeSessionRequest` driving a `RetryDueEvent`)
  re-queries with backoff and, at `maxResolveRetries`, fails the
  session terminally so it becomes reap-eligible and frees its
  `r.incoming` concurrency slot. Without this an unanswered resolve
  would pin a child forever.

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
  `IncomingSnapshot.MetadataAttempts uint32` — persisted retry count for
  authoritative metadata resolution (phase-2 indexer query). Drives bounded
  exponential backoff and terminal give-up in `handleReceiveOutboxError`
  across restarts so a session whose VTXO never lands in the indexer stops
  re-querying forever. Serialized as TLV record 19.
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
  (`TimeoutActor`), `lnd/input` (signer interface for inline checkpoint /
  Ark signing on the session actor's turn).
- **Depended on by**: `darepod`.
- **Sends**:
  - → `serverconn`: `SendSubmitPackageRequest`,
    `SendFinalizePackageRequest`, `SendIncomingAckRequest`.
  - → `serverconn` durable mailbox:
    `QueryIncomingTransferRequest` →
    `SendListOORRecipientEventsByScriptRequest`;
    `QueryIncomingMetadataRequest` →
    `SendListVTXOsByScriptsRequest`.
  - → `db` (via outbox): `MarkInputsSpentRequest`.
  - → `wallet`: `MaterializeIncomingVTXOsRequest`.
  - → `vtxo` manager: `VTXOsMaterializedNotification`.
  - → `ledger` (when `LedgerSink` is `fn.Some`): `VTXOSentMsg` on
    `FinalizeAcceptedEvent`; `VTXOReceivedMsg{Source=SourceOOR}` per
    materialized descriptor. Told inside the commit transaction so
    the accounting lands atomically with the session snapshot.
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
- Transport events (submit / finalize / ack) are delivered directly
  into the `serverconn` durable actor during the commit transaction:
  serverconn is durable, so each `Tell` persists into its mailbox via
  the ambient OOR turn tx and the message lands IFF the turn commits.
  The wire send runs later on serverconn's own egress turn, outside
  the OOR tx, and is retried by serverconn — no separate outbox
  publisher hop. (The generic outbox publisher is still wired for the
  registry's durable ask-response handoff, not for transport.)
- Outgoing finalize ordering: local input-spend completion runs inline
  in dispatch with **no OOR writer tx held**, before the FSM advances to
  `Completed` and before the package write is staged. The VTXO manager's
  status write commits in the VTXO actor's own transaction (a second
  writer), so it does **not** join the OOR turn tx; awaiting it under a
  held OOR writer lock would deadlock on the single SQLite/Postgres
  writer. Completion is non-atomic with the OOR snapshot but re-driven
  idempotently on boot (resume re-emits `MarkInputsSpentRequest`;
  `isPersistedSpent` absorbs the replay).
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
- Snapshots version per direction: `OutgoingSnapshot.Version = 4`,
  `IncomingSnapshot.Version = 1` (each serialized as TLV record type 1).
  Restore requires a non-zero version (`snapshot version must be provided`).
- Self-transfer: a `ResolveIncomingTransferRequest` for a session
  with an active outgoing session errors until the outgoing session
  terminates; then the outgoing entry is deleted and an incoming
  session is created in its place.
- Signing is inline and durable-by-construction: the session actor
  signs Ark and checkpoint PSBTs within its Read/Commit turn, so the
  signed transport outbox is persisted in the same transaction as the
  FSM advance. A restart-duplicate event that reaches the actor after
  the FSM has advanced is silently discarded by `DriveEventRequest`.
- `ReceiveLimits` are propagated through the `newOORActorCodec` factory
  so every deserialized message enforces the same caps as the in-memory
  path. The codec instance is shared per actor.
- `StartTransferRequest.IdempotencyKey`: when non-empty, the registry
  dedups admission against the durable store via
  `LookupActiveSessionByIdempotencyKey`, returning
  `StartTransferResponse{Existing: true}` on hit. Failed sessions
  never answer for a key (a partial UNIQUE index on
  `oor_session_registry` enforces at most one live-or-completed row
  per key), so a keyed retry after a failure admits a fresh session.
  Empty key preserves the historical deterministic (Ark txid) session.
- Phantom-resident dedup guard: `handleStartTransfer` answers
  `Existing: true` for a resident outgoing child ONLY after confirming a
  durable row backs it via `GetSession`. On the production (detachable)
  path a failed admission is reaped asynchronously by a
  `SessionTerminalNotification`, so the row-less child lingers in
  `r.active` until that notification is processed. Deduping against it
  would wedge a same-input retry (sessionID = Ark txid): the follow-up
  `DriveEvent` restores nothing and errors as an unknown session. On a
  not-found row the registry drops the phantom synchronously on its own
  goroutine and falls through to a fresh admission.
- Bounded detached-continuation wait: the promise handoff parks an
  `OnComplete` goroutine on the caller's context, which unblocks only on
  the child future resolving or the caller context being done. The
  production StartTransfer call site derives its context from
  `context.WithoutCancel`, so that caller context never cancels; a wedged
  or never-resolving child turn would leak the continuation for the
  daemon's lifetime. `completeAdmissionHandoff` and `routeAsk` therefore
  wrap `detachedAsk.CallerCtx` in `context.WithTimeout(detachedWaitTimeout)`
  (5m; a behavior field shrinks it in tests) before handing it to
  `OnComplete`, so the goroutine always exits. The phantom-reap guard in
  `completeAdmissionHandoff` keys off the wrapped wait context's error,
  not the raw CallerCtx, so a deadline-exceeded wait (a wedged child) is
  treated like a benign caller hang-up and does NOT reap a session that
  may still be signing under its own receive-loop context. On the
  registry's durable (Read/Stage/Commit) path `DetachedAsk.CallerCtx` is
  the registry ACTOR's lifetime context, not the originating caller's (the
  durable mailbox does not persist the caller's context with the Ask), so a
  caller deadline never propagates into the detached continuation — it is
  observed only by the caller's own `future.Await`. The
  `context.WithTimeout(detachedWaitTimeout)` wrap is therefore the SOLE
  bound on the continuation.
- Incoming concurrency cap (`MaxConcurrentIncomingSessions`) is
  enforced in `ensureChild`, the choke point every resident-making
  path funnels through (admission, lazy restore on a routed message,
  boot restore), so the bound holds even on paths that skip
  `handleResolveIncoming`'s pre-spawn check. `restoreNonTerminal`
  restores oldest-first and treats an over-cap incoming row as a
  non-fatal skip, so a corrupted backlog of more than the cap of
  non-terminal incoming rows cannot wedge the subsystem on every boot;
  outgoing sessions carry no cap.
- Terminal row retention: terminal rows (completed and failed, all
  directions) are retained in `oor_session_registry` so failed sessions
  stay visible to status RPCs and for diagnostics; `handleSessionTerminal`
  only reaps the in-memory child, it does not delete the row.
  `MaxConcurrentIncomingSessions` bounds the resident children an operator
  can pin; a future bounded-retention sweep, if needed, should age out all
  terminal rows uniformly rather than deleting one class at reap time.
- Idempotency-key dedup race: on Postgres the racing children do not
  serialize on `oor_session_registry`, so the loser's snapshot upsert
  collides on the partial UNIQUE index. `commitAck` returns that error so
  the turn rolls back and redelivers; the redelivered `resolveKeyDedup` --
  running in a fresh tx where the winner's row is now committed -- sees the
  winner and consumes cleanly as `Existing` (no special error
  classification: any commit error redelivers, and the partial UNIQUE
  index is the safety net against a duplicate row). The dedup loser wrote
  no durable row, so the clean-dedup turn fires a
  `SessionTerminalNotification`: the registry's reaper treats a no-row
  session as reap-eligible and drops the orphaned child (goroutine,
  mailbox, receptionist key) instead of leaking it until shutdown.
- Duplicate drive-event after reap: a late at-least-once duplicate
  server push for a session that has reached a terminal snapshot and
  been reaped misses the ingress fast path (key unregistered) and
  routes through the registry. `handleDriveEvent` distinguishes a
  present-but-terminal row (`sessionIsTerminal`) from a truly-unknown
  one: the terminal case acks cleanly as an idempotent no-op, only a
  genuinely-unknown session errors. Without this a normal duplicate
  would Nack, retry to the cap, and dead-letter.
- Registry `Stop` runs `stopChildren` (which iterates the
  unsynchronized `r.active` map) ONLY when the bounded `StopAndWait`
  drain returns nil -- the path where `process()` has provably exited.
  On a drain timeout the registry turn may still be mutating the map,
  so `stopChildren` is skipped (children are torn down by actor-system
  shutdown) to avoid a fatal concurrent map iteration and write.

## Deep Docs

- [oor/doc.go](doc.go) — Package overview.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
