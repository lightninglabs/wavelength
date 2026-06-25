# oor

## Purpose

Client-side out-of-round (OOR) VTXO transfer coordination without waiting for
normal rounds, preserving deterministic transaction construction and crash-safe
resume semantics.

## Key Types

For field-level detail, use `go doc github.com/lightninglabs/darepo-client/oor.<Symbol>`.
State transitions and validation rules live under [Invariants](#invariants).

### Per-Session Actor Model (current)

The daemon runs **one durable actor per OOR session** instead of one global
actor. See [docs/oor_subsystem.md](../docs/oor_subsystem.md) for the full design.

- `OORSessionActor` / `sessionBehavior` — one durable actor per session on the
  Read/Commit execution path. The FSM emits outbox events, and the actor handles
  them in `driveOutboxEvents` (a single shared switch): sign inline, enqueue
  cross-actor transport to serverconn, stage materialization into the commit
  transaction, schedule retries. There is no separate `OutboxHandler` for
  transport or signing; only local-persistence effects (materialization) are
  delegated to `IncomingHandler`.
- `OORRegistryActor` / `oorRegistryBehavior` — thin coordinator registered under
  the OOR service key (`oor-client`) with a **durable inbound mailbox**: a
  server-push event is persisted before the ingress loop acks the operator
  envelope, so a crash between ingress and the per-session child replays the
  registry's idempotent spawn+forward instead of losing the event. The registry
  routes each message to the right session's child (hot-path `DriveEvent` via
  Tell; `StartTransfer`/`GetState` via promise handoff — the registry detaches
  the caller's promise with `actor.DetachAskPromise` and the child's result
  settles it through `OnComplete`, so the registry goroutine never parks on a
  child's signing turn and concurrent admissions sign in parallel), dedups
  outgoing transfers by idempotency key, lazily spawns children, routes
  retry-timer `ResumeSessionRequest` expiries to the owning child
  (unknown/terminal sessions are benign no-ops), reaps children on
  `SessionTerminalNotification`, and `RestoreNonTerminal` respawns **and
  resumes** in-flight sessions on boot: the restore runs as a registry message
  (`RestoreNonTerminalRequest`) on the registry goroutine, and each restored
  child is told a `ResumeSessionRequest` so it re-drives the outbox implied by
  its restored state (retry timers are in-memory and do not survive restarts).
- `ActorIDForSession` / `SessionServiceKey` / `SessionRegistryStore` —
  deterministic per-session mailbox id (`oor-session-<hex>`), the per-session
  receptionist key the registry registers each live child under (the ingress fast
  path resolves it to tell `DriveEventRequest`s straight into the child's durable
  mailbox, falling back to the registry on a miss), and the control-plane store.
  A session's full durable state lives in one `oor_session_registry` row
  (queryable columns + an opaque resume snapshot); OOR does not use the generic
  `fsm_checkpoints` blob.
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
  event is handled inline by the session actor's own `driveOutboxEvents` switch.
- `SignArkPSBT` — signs Ark PSBT inputs on the checkpoint 2-of-2 collab
  leaf using `MultiPrevOutFetcher` for BIP-341 sighashes across
  multi-input transfers. Signing runs inline on the session actor's turn;
  there is no separate signing actor.
- `ReceiveLimits` / `DefaultReceiveLimits` — defense-in-depth bounds on
  incoming receive (`MaxCheckpoints=64`, `MaxVTXOMatches=128`,
  `MaxMailboxItems=10000`, `MaxMailboxScriptBytes=10000`,
  `MaxConcurrentIncomingSessions=1024`). Zero fields are normalized; the
  `newOORActorCodec` factory captures limits so deserialization enforces them.
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

### `sessionBehavior` internals

Per-turn accumulators are reset at the top of `Receive` and consumed by
`commitAck`:

- `pendingTransport []serverconn.ServerConnMsg` — cross-actor transport
  messages collected by `driveOutboxEvents`; `commitAck` enqueues them
  durably into serverconn in the same writer transaction as the snapshot
  and ack so the wire send is IFF the turn commits.
- `pendingLedger []ledger.LedgerMsg` — accounting messages; same atomic
  delivery via the ledger actor's durable mailbox in `commitAck`.
- `commitWork []func(ctx, oorTx) error` — durable writes (reservations,
  finalized package, incoming materialization) staged during dispatch
  and run inside `commitAck`'s writer transaction.
- `postCommit []func(ctx)` — best-effort cross-actor notifications (VTXO
  manager, observer) run after the turn commits; run on a goroutine with
  a daemon-owned context so a terminal turn's reap doesn't cancel them.
- `pendingRetries []*ScheduleRetryRequest` — timer arms deferred to
  after commit so a rolled-back turn never schedules a timer.
- `terminalCommitted bool` — set by `commitAck` when the snapshot is
  terminal; triggers `notifyTerminal` after `Receive` returns.
- `commitFailed bool` — set when dispatch advances the in-memory FSM but
  the subsequent Commit rolls back; causes the next redelivered turn to
  call `restore()` before dispatch so the FSM re-aligns with the durable
  row.

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
- `SessionTerminalNotification` — TLV-durable (`0x7019`); sent from
  session child to registry after a terminal commit so the registry
  reaps the in-memory child.
- `RestoreNonTerminalRequest` — TLV-durable (`0x701a`); sent by
  `RestoreNonTerminal` into the registry mailbox to serialize boot
  restore with any redelivered backlog.
- `ResumeSessionRequest` — TLV-durable (`0x7014`); routed by the
  registry to a child to re-drive the outbox implied by current state.
  `FromRetryTimer bool` distinguishes a fired timer (advances give-up
  counter) from a boot restore (only re-arms the timer).

### Outbox Events

- `RequestArkSignatures` — sign Ark PSBT inline; drives `ArkSignedEvent`.
- `RequestCheckpointSignatures` — sign checkpoint PSBTs inline; drives
  `CheckpointsSignedEvent`.
- `SendSubmitPackageRequest` / `SendFinalizePackageRequest` — collected
  into `pendingTransport`; enqueued durably into serverconn by `commitAck`.
- `QueryIncomingTransferRequest` — emitted after persisting
  `ReceiveResolving`; mapped to
  `serverconn.SendListOORRecipientEventsByScriptRequest`.
- `QueryIncomingMetadataRequest` — emitted after
  `IncomingTransferEvent`; mapped to
  `serverconn.SendListVTXOsByScriptsRequest`.
- `MaterializeIncomingVTXOsRequest` — staged into `commitWork`; run
  inside the commit transaction via `materializeIncoming`, which drives
  the FSM further (to `IncomingHandledEvent`) before the snapshot is
  taken.
- `SendIncomingAckRequest` — collected into `pendingTransport`; also
  drives `IncomingAckSentEvent` in-turn to advance the FSM to
  `ReceiveCompleted`.
- `MarkInputsSpentRequest` — runs `completeSpend` inline in dispatch
  (no OOR writer held) before driving `InputsMarkedSpentEvent`.
- `ReleaseInputsRequest` — best-effort `releaseSpend` inline; session
  is already terminal failed.
- `ScheduleRetryRequest` — queued into `pendingRetries`; armed after
  commit via the timeout actor.
- `IncomingTransferNotification` — informational; logged only.

### FSM Events & Incoming Receive States

- Events: `IncomingTransferEvent`, `IncomingMetadataResolvedEvent`,
  `IncomingHandledEvent`, `IncomingAckSentEvent`, `RetryDueEvent`.
- `ReceiveState`: `ReceiveIdle` → `ReceiveResolving` (durable hint
  persisted, waiting for phase-1 indexer outside the actor tx) →
  `ReceiveNotified` (package received, awaiting materialization) →
  `ReceiveAwaitingAck` (materialized, awaiting transport ack) →
  `ReceiveCompleted`. `ReceiveResolving` arms a give-up timer
  alongside its phase-1 query (`ResolveAttempts`, persisted): the
  phase-1 query has no failure response on operator silence, so each
  timer expiry (a `ResumeSessionRequest{FromRetryTimer:true}` driving a
  `RetryDueEvent`) re-queries with backoff and, at `maxResolveRetries`,
  fails the session terminally so it becomes reap-eligible and frees its
  `r.incoming` concurrency slot.

### Outbox Handler Chain & Callbacks

- `LocalPersistenceOutboxHandler` — handles `MarkInputsSpentRequest`,
  `QueryIncomingMetadataRequest`, `MaterializeIncomingVTXOsRequest`,
  `SendIncomingAckRequest`; delegates everything else to `Next`. Also
  implements `IncomingMetadataRecipientFilter` so the transport layer
  can pre-filter owned recipients.
- `SpendCompleter` — `func(ctx, []wire.OutPoint) error` routing OOR
  spend completion through the VTXO manager. `nil` ⇒ direct store
  writes (migration compat).
- `SpendReleaser` — `func(ctx, []wire.OutPoint) error` returning
  reserved inputs from SpendingState to LiveState on a pre-point-of-
  no-return terminal failure.
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
  `IncomingSnapshot.MetadataAttempts uint32` — persisted retry count
  for authoritative metadata resolution (phase-2 indexer query). Drives
  bounded exponential backoff and terminal give-up in
  `handleReceiveOutboxError` across restarts so a session whose VTXO
  never lands in the indexer stops re-querying forever. Serialized as
  TLV record 19.
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

## Actor Model: Session Creation and Registry Coordination

### Outgoing session lifecycle

1. Caller Asks the registry with `StartTransferRequest`.
2. Registry calls `NewSessionWithIdempotencyKey` to derive the session id
   (Ark txid), dedups against the store, calls `ensureChild`, and forwards
   the Ask to the child. The `pendingHandoff` is consumed in `Receive` after
   the registry's consuming Commit succeeds; the caller's promise is detached
   onto the child's future via `DetachAskPromise`.
3. The child's `handleStartTransfer` builds the FSM, stages reservations in
   `commitWork`, calls `driveOutboxEvents` (which signs the Ark PSBT inline and
   collects `SendSubmitPackageRequest` into `pendingTransport`), then
   `commitAck` writes the snapshot, enqueues transport, and ledger atomically.
4. Operator response (`SubmitAcceptedEvent`, `FinalizeAcceptedEvent`) arrives
   via the ingress fast path (child's per-session service key) as a
   `DriveEventRequest`; the child's `handleDriveEvent` drives the FSM and
   handles effects (inline checkpoint signing, spend completion, package
   persistence).
5. On a terminal commit the child calls `notifyTerminal` (goroutine Tell); the
   registry's `handleSessionTerminal` reaps the child.

### Incoming session lifecycle

1. Operator server-push is persisted into the registry's durable mailbox as
   `ResolveIncomingTransferRequest`.
2. Registry's `handleResolveIncoming` validates ownership, enforces the
   concurrency cap, calls `ensureChild`, and Tells the child.
3. Child's `handleResolveIncomingTransfer` creates the FSM in `ReceiveResolving`
   and drives its initial outbox (phase-1 indexer query into serverconn).
4. Phase-1 response arrives as `DriveEventRequest{IncomingTransferEvent}`;
   the child validates the package graph and drives the FSM to
   `ReceiveNotified`, staging materialization into `commitWork`.
5. Inside `commitAck`, `materializeIncoming` writes VTXO rows and drives the
   FSM to `ReceiveAwaitingAck`; the ack transport is collected and enqueued.
6. Terminal commit notifies the registry; the session's incoming slot is freed.

### Boot restore

`RestoreNonTerminal` sends a `RestoreNonTerminalRequest` into the registry
mailbox. `restoreNonTerminal` loads non-terminal rows oldest-first, calls
`ensureChild` for each (skipping over-cap incoming rows rather than aborting),
then Tells each child a `ResumeSessionRequest{FromRetryTimer:false}` to
re-drive the outbox implied by its restored state.

## Message Flows (Tell/Ask Patterns)

| Caller → Target | Pattern | Message |
|-----------------|---------|---------|
| API → registry | Ask | `StartTransferRequest` |
| Registry → child | Ask (detached) | `StartTransferRequest` |
| Ingress fast path → child | Tell | `ResolveIncomingTransferRequest`, `DriveEventRequest` |
| Registry → child | Tell | `ResolveIncomingTransferRequest`, `DriveEventRequest`, `ResumeSessionRequest` |
| Child → serverconn (in commitAck tx) | Tell | `SendSubmitPackageRequest`, `SendFinalizePackageRequest`, `SendIncomingAckRequest`, `SendListOORRecipientEventsByScriptRequest`, `SendListVTXOsByScriptsRequest` |
| Child → ledger (in commitAck tx) | Tell | `VTXOSentMsg`, `VTXOReceivedMsg` |
| Child → registry (goroutine, advisory) | Tell | `SessionTerminalNotification` |
| Registry → self (goroutine, self-transfer redrive) | Tell | `ResolveIncomingTransferRequest` |
| Child → vtxoManager (post-commit goroutine) | Tell | `VTXOsMaterializedNotification` |
| API → registry | Ask | `GetStateRequest` → forwarded to child |
| API → registry | Ask | `ListSessionsRequest` → served from store directly |
| Caller → registry | Ask | `RestoreNonTerminalRequest` |
| Timeout actor → registry (via `NewRetryCallbackRef`) | Tell | `ResumeSessionRequest{FromRetryTimer:true}` |

## Inbound Durable Mailbox Mechanics

The registry actor's mailbox id is `oor-client` (the legacy global actor id,
preserved so pre-cutover unacked rows drain through the same surface after
upgrade). Each per-session child uses id `oor-session-<hex-session-id>`.

Every message in `OORDurableMsg` implements `actor.TLVMessage` for durable
storage. The `newOORActorCodec` factory creates a codec that captures
`ReceiveLimits` so every deserialization enforces the same caps as the
in-memory path.

The Read/Stage/Commit lifecycle (`actor.TxBehavior`) works as follows:
- **Read**: the framework deserializes the next unacked message and calls
  `Receive`.
- **Stage** (implicit): `dispatch` runs all inline effects and populates
  accumulators; no DB write yet.
- **Commit**: `ax.Commit(...)` opens a writer transaction, runs `commitWork`
  closures (materialization, reservations, package), writes the snapshot via
  `RegistryStore.UpsertSession`, Tells transport into serverconn's durable
  mailbox, Tells ledger entries, then folds the ack watermark. All of this is
  one atomic writer transaction.

If the Commit fails (lease lost or DB error), `commitFailed` is set and the
redelivered turn calls `restore()` to re-align the in-memory FSM with the
last-committed row before re-applying the event.

## Imports: Dependencies and Dependents

### oor/ depends on

- `baselib/actor` — durable actor, TxBehavior, ServiceKey, DetachAskPromise
- `baselib/protofsm` — FSM, StateTransition, EmittedEvent
- `db` (`clientdb`) — OORSessionRegistryRecord, OORSessionDirectionOutgoing/Incoming, VTXOStore
- `serverconn` — SendSubmitPackageRequest, SendFinalizePackageRequest, SendListOORRecipientEventsByScriptRequest, SendListVTXOsByScriptsRequest, SendIncomingAckRequest
- `ledger` — Sink, VTXOSentMsg, VTXOReceivedMsg
- `timeout` — ScheduleTimeoutRequest, ExpiredMsg
- `vtxo` — VTXOStore, ManagerMsg, VTXOsMaterializedNotification, Descriptor, Ancestry
- `lib/arkscript` — CheckpointPolicy
- `lib/tx/oor` (oortx) — RecipientOutput
- `lnd/input` — Signer interface for inline checkpoint/Ark signing

### Depended on by

- `darepod` — wires `OORRegistryActor` into the daemon, calls
  `RestoreNonTerminal` at boot, exposes OOR RPCs. Files:
  `server.go`, `rpc_server.go`, `wallet_ops.go`, `config.go`,
  `incoming_metadata.go`, `incoming_ancestry_fetcher.go`,
  `receive_script.go`, `rpc_operation_status.go`, `wallet_recovery.go`,
  `logging.go`.

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
  publisher hop.
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
  terminates; the hint is parked in `parkedSelfHints` for an
  event-driven redrive at reap, with the durable delivery nacking on a
  30-second flat backoff as crash-safety fallback.
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
- `commitFailed` flag: set when `dispatch` advances the in-memory FSM
  but the Commit rolls back (a non-lease-loss error). The next driving
  turn reloads from the durable row via `restore()` before dispatch so
  the redelivered event re-applies against the last-committed state
  rather than being silently no-op'd by an uncommitted advance. A
  lease-loss commit failure never sets this: the fencing instance owns
  the state going forward.

## Deep Docs

- [oor/doc.go](doc.go) — Package overview.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
