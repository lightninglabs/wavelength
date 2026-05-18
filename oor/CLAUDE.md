# oor

## Purpose

Client-side out-of-round (OOR) VTXO transfer coordination without waiting for
normal rounds, preserving deterministic transaction construction and
restart-safe resume semantics.

The package now uses in-memory actors/coordinators only as concurrency and
callback boundaries. Durable state lives in SQL session/effect tables and in
domain stores; there is no generic durable-actor checkpoint fallback for OOR.

## Core Model

- `SessionID` is the stable session identifier, derived from the Ark txid in v0.
- `StateMachine`/`ReceiveStateMachine` are the deterministic protocol FSMs.
- `OORClientActor` is an in-memory actor facade over per-session FSMs. It
  requires `ClientActorCfg.SessionStore` and restores active sessions from SQL
  at startup.
- `ClientCoordinator` is the actorless facade over the same behavior. It is used
  where callers want synchronous channel-style coordination without actor
  mailbox semantics.
- `OORClientSessionStore` is the durable boundary for outgoing sessions,
  incoming sessions, pending incoming hints, and OOR client effect rows.
- `OORClientEffectWorker` claims SQL effect rows and executes retryable local
  effects such as signing, incoming metadata resolution, materialization, and
  incoming ACK sends.
- `OutboxHandler` executes FSM outbox requests. The standard chain is
  `LocalPersistenceOutboxHandler -> SigningOutboxHandler`.

## Important Types

- `OutgoingSnapshot` captures one outgoing FSM session as normalized state for
  SQL persistence and effect replay. It is still TLV encoded when embedded in
  explicit messages, but it is not wrapped in a generic actor checkpoint blob.
- `IncomingSnapshot` captures one incoming receive FSM session for SQL
  persistence and diagnostics.
- `TransferInputSnapshot` stores the client-side signing context required to
  resume checkpoint and Ark signing byte-identically after restart.
- `ReceiveLimits` / `DefaultReceiveLimits` bound incoming OOR payload shapes:
  checkpoints, VTXO matches, mailbox items, and mailbox script bytes.
- `FindOutgoingSessionByIdempotencyKeyRequest` lets callers locate an existing
  outgoing transfer by caller-provided idempotency key.
- `ListSessionsRequest` returns an in-memory diagnostic projection of active
  sessions; persisted restart state is loaded through `OORClientSessionStore`.

## Outbox Flow

Outgoing send:

- FSM emits `RequestArkSignatures`.
- Signing runs through `SigningOutboxHandler` or SQL client effects.
- Transport outbox events (`SendSubmitPackageRequest`,
  `SendFinalizePackageRequest`) are routed through `serverconn`.
- Finalized outgoing packages are persisted through `PackagePersistence` before
  ledger send notifications are emitted.

Incoming receive:

- `ResolveIncomingTransferRequest` records a durable pending hint.
- `QueryIncomingTransferRequest` asks the server/indexer for the full incoming
  Ark package.
- `QueryIncomingMetadataRequest` resolves authoritative VTXO metadata.
- `MaterializeIncomingVTXOsRequest` persists local incoming VTXOs.
- `SendIncomingAckRequest` acknowledges the receive to the server.

All of those phases are restart-safe through SQL session rows and effect rows,
not through an actor checkpoint blob.

## Relationships

- Depends on `baselib/protofsm` for FSM execution and `baselib/actor` only for
  in-memory actor wiring.
- Depends on `serverconn` for durable transport delivery to the server.
- Depends on `ledger` for best-effort OOR transfer accounting notifications.
- Depends on `timeout` for retry scheduling.
- Used by `darepod` wiring and tests.

## Invariants

- Checkpoint output collaborative path is 2-of-2
  `MultiSigCollabTapLeaf(clientKey, operatorKey)`.
- At submit time only structural validation runs; full script validation
  requires both signatures and runs at finalize.
- Point-of-no-return is the server/operator checkpoint co-signature. After this,
  retries must return byte-identical co-signed checkpoint PSBTs.
- Incoming receive never performs synchronous server/indexer RPCs inside local
  persistence writes; queries and materialization are explicit outbox/effect
  stages.
- `OORClientActor` and `ClientCoordinator` must be constructed with
  `SessionStore`; no `SessionStore` is a startup error.
- Ancestor packages from incoming OOR events must be persisted before incoming
  materialization is made visible, so multihop recovery can rebuild lineage
  after restart.
- `ReceiveLimits` are enforced by adapters and codecs before unbounded payloads
  are materialized.

## Deep Docs

- [doc.go](doc.go) - Package overview.
- [../ARCHITECTURE.md](../ARCHITECTURE.md) - System-wide package map.
