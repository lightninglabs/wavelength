# oor

## Purpose

Client-side out-of-round (OOR) VTXO transfer coordination: lets a client send
VTXOs to one or more recipients without waiting for a normal round, while
keeping transaction construction deterministic and resume semantics
crash-safe. Built on `baselib/protofsm`: I/O is modeled as outbox requests
that a durable actor executes and feeds back as events.

## Key Types

For field-level detail, use `go doc github.com/lightninglabs/wavelength/oor.<Symbol>`.

- `OORSessionActor` — one durable actor per OOR session (outgoing or
  incoming). Its `driveOutbox` switch handles every outbox event inline
  (signs Ark/checkpoint PSBTs, enqueues transport to `serverconn`,
  materializes incoming VTXOs, schedules retries) on the Read/Commit turn;
  there is no separate signing actor.
- `OORRegistryActor` — durable coordinator registered under the OOR service
  key. Routes messages to the owning session's child, lazily spawns
  children, dedups outgoing transfers by idempotency key, reaps terminated
  children, and respawns/resumes non-terminal sessions on boot.
- `Session` / `ReceiveSession` — outgoing/incoming FSM state containers;
  `OutgoingSnapshot` / `IncomingSnapshot` are their durable, versioned
  serializations.
- `OutboxHandler` / `LocalPersistenceOutboxHandler` — handles the local
  persistence outbox events (mark-inputs-spent, incoming metadata query,
  VTXO materialization, ack); everything else is handled inline by the
  session actor.
- `ReceiveLimits` / `DefaultReceiveLimits` — defense-in-depth bounds on
  incoming receive (`MaxCheckpoints`, `MaxVTXOMatches`, `MaxMailboxItems`,
  `MaxMailboxScriptBytes`, `MaxConcurrentIncomingSessions`).
- `ErrIdempotencyKeyConflict` — caller-visible outgoing admission conflict
  when a deterministic session cannot retain the supplied key; `waved` maps it
  to `codes.AlreadyExists` after releasing freshly selected inputs.

## Relationships

- **Depends on**: `baselib/protofsm` (FSM), `baselib/actor` (durable actor
  framework), `serverconn` (submit/finalize/query transport), `vtxo`
  (materialization + status), `ledger` (`Sink`, accounting emission),
  `timeout` (`TimeoutActor` retry scheduling), `lib/arkscript` (checkpoint
  policy, collab tapleaf), `arkrpc` (indexer response types), `lnd/input`
  (signer interface for inline Ark/checkpoint signing).
- **Depended on by**: `waved` (spawns the registry, wires config, drives
  RPCs and event routing).
- **Messages to/from**: Sends `SendSubmitPackageRequest` /
  `SendFinalizePackageRequest` / `SendIncomingAckRequest` and durable query
  requests (`QueryIncomingTransferRequest`, `QueryIncomingMetadataRequest`)
  -> `serverconn`; `MaterializeIncomingVTXOsRequest` -> wallet/VTXO store;
  `VTXOSentMsg`/`VTXOReceivedMsg` -> `ledger` (when `LedgerSink` is set).
  Receives `SubmitAcceptedEvent` / `FinalizeAcceptedEvent` /
  `ResolveIncomingTransferRequest` <- `serverconn` event router;
  `StartTransferRequest` / `DriveEventRequest` / `ListSessionsRequest` <-
  `waved` RPC layer.

## Invariants

- Checkpoint collab output is 2-of-2
  (`arkscript.MultiSigCollabTapLeaf(clientKey, operatorKey)`), never
  single-sig; resumed custom-spend inputs are re-verified against the VTXO
  pkScript before signing.
- Point-of-no-return is server co-signing of the checkpoint transaction(s):
  after that, the client must resume with byte-identical co-signed PSBTs
  (deterministic construction), not re-derive them.
- Signing is inline and durable-by-construction: the session actor signs
  within its Read/Commit turn, so the signed transport outbox commits in the
  same transaction as the FSM advance.
- Transport sends (submit/finalize/ack) are delivered into `serverconn`'s
  durable mailbox inside the OOR commit transaction; the actual wire send
  happens later on serverconn's own egress turn and is retried there — OOR
  does not run a separate outbox publisher for transport.
- Incoming receive never performs a synchronous unary RPC inside the durable
  actor's DB transaction; both phase-1 hint resolution and phase-2
  authoritative metadata lookup go through durable `serverconn` query
  messages and return as fresh events.
- Snapshots are versioned per direction (`OutgoingSnapshot.Version = 5`,
  `IncomingSnapshot.Version = 1`); restore rejects a zero version. Outgoing
  v5 adds the `FirstRejectUnixNanos` record (bounded transient submit-reject
  retry window); a pre-v5 snapshot decodes it to 0 (a fresh window).
- `StartTransferRequest.IdempotencyKey` dedup reads
  `oor_dispatch_attempts`, not the mutable session row. The table has one row
  per caller key and one caller key per deterministic session id. Once the
  first submit is durably queued, terminal failure and incoming self-transfer
  progress cannot release or replace that identity.
- After deterministic session construction, `handleStartTransfer` rejects a
  non-empty caller key when the same session id is pending or durably bound
  under another key, or when a durable keyless session already owns that id.
  It rejects before forwarding to the child so a caller never receives success
  under a key that exact replay cannot find. The narrow exception is a failed
  outgoing legacy/keyless row with no immutable attempt: a keyed retry rebuilds
  the same deterministic session, and its first new submit checkpoint binds
  the key and request before transport enqueue.
- `SendOOR` builds `DispatchRequestData` where both caller recipients and the
  optional wallet change output are known. The proof binds only caller
  recipients to their positions in the full canonical output list, then rides
  the durable start message and snapshot to the first submit-capable state.
  `UpsertSession` inserts it with the immutable attempt in the same transaction
  as the snapshot and first transport enqueue. Exact replay rejects a changed
  caller count, amount, or script. Later states cannot replace the proof.
- `MaxConcurrentIncomingSessions` (default 1024) is enforced in the
  registry's `ensureChild` choke point, the only path that makes a session
  resident, so every admission path (RPC, routed message, boot restore)
  shares the same bound.
- Witness/script decode bounds mirror consensus limits:
  `maxConditionWitnessItems = 64` items of at most 520 bytes each (Bitcoin's
  `MAX_SCRIPT_ELEMENT_SIZE`), enforced on both encode and decode.
- Terminal rows (completed and failed) are retained in
  `oor_session_registry` for status/diagnostics; reaping only removes the
  in-memory child, never the row.
- Outgoing finalize ordering: input-spend completion runs inline with no OOR
  writer transaction held, because its write commits in the VTXO manager's
  own transaction; awaiting that second writer under a held OOR writer lock
  would deadlock the single SQLite/Postgres writer.
- The registry's detached-continuation wait on a spawned child (`OnComplete`)
  is bounded solely by wrapping `DetachedAsk.CallerCtx` in
  `context.WithTimeout(detachedWaitTimeout)` (5m); the phantom-reap guard
  keys off that wrapped context's error, not the raw caller ctx, so a
  timed-out wait is treated as a benign hang-up and never reaps a
  still-signing session.
- Idempotency-key dedup on Postgres is a commit-race, not only a pre-check:
  losing children collide on a unique identity constraint in `commitAck`,
  roll back, and redeliver. The redelivered `resolveKeyDedup` sees the
  immutable winner and consumes cleanly as `Existing`.
- `handleStartTransfer` answers `Existing: true` for a resident outgoing
  child only after confirming a durable row via `GetSession`; a row-less
  phantom (pending async reap via `SessionTerminalNotification`) is dropped
  synchronously and falls through to a fresh admission instead of wedging a
  same-input retry.
- A late duplicate server push for a terminal, already-reaped session routes
  through the registry; `handleDriveEvent` acks it as an idempotent no-op
  (`sessionIsTerminal`) rather than erroring, since only a genuinely-unknown
  session should Nack.
- Incoming resolve/metadata retries give up terminally once their persisted
  attempt counts reach `maxResolveRetries` / `maxMetadataRetries` (20),
  freeing the session's concurrency slot instead of pinning a child forever
  on operator silence.
- Incoming ancestor packages are capped at `maxAncestorPackages = 64`
  checkpoints, and indexer-supplied `tree_depth` is cross-checked against the
  reconstructed path via `arkrpc.ValidateAncestryPathDepth`.
- Server-side lineage-cap rejection surfaces as a typed `*ErrLineageTooLarge`
  via `ClassifySubmitError`, so wallet callers can switch on the cause
  without depending on the `oorpb` proto type.

## Deep Docs

- [oor/doc.go](doc.go) — Package overview.
- [docs/oor_subsystem.md](../docs/oor_subsystem.md) — Per-session actor
  design in full.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
