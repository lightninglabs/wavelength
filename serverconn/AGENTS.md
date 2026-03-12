# serverconn

## Purpose

Unified connector for all mailbox traffic between client and remote Ark server,
combining durable egress (crash-safe events), low-latency unary RPCs, and
background ingress polling with event routing.

## Key Types

- `Runtime` — Main entry point wrapping DurableActor, ServerConnectionActor, and UnaryFacade.
- `ServerConnectionActor` — Core behavior handling egress messages and the ingress loop.
- `UnaryFacade` — Implements `mailboxrpc.RPCClient` for generated RPC stubs (low-latency path).
- `ConnectorConfig` — Wiring configuration (edge address, mailbox IDs, dispatchers, store).
- `AckState` — Four-cursor watermark state machine (PullCursor, DispatchCommittedTo, AckTarget, AckCommittedTo).

## Relationships

- **Depends on**: `baselib/actor` (DurableActor infrastructure), `mailbox/*` (Envelope, RpcMeta, MailboxServiceClient).
- **Depended on by**: `round` (outbound RPCs), `oor` (durable transport), `darepod` (wiring).
- **Sends (egress → remote mailbox)**:
  - `SendClientEventRequest` (durable): wraps `JoinRoundRequest`, `SubmitNoncesRequest`, `SubmitPartialSigRequest`, `SubmitForfeitSigRequest`
  - `SendRPCRequest` (unary, non-durable): low-latency request-response RPCs
- **Routes (ingress → local actors via EventRouter)**:
  - → `round`: `CommitmentTxBuilt`, `NoncesAggregated`, `OperatorSigned`, `RoundJoined`, `BoardingFailed`
  - → `oor`: `SubmitAcceptedEvent`, `FinalizeAcceptedEvent`, `IncomingTransferEvent`
- **Receives (from local actors for outbound delivery)**:
  - ← `round`: `SendClientEventRequest` (outbox messages for persistence)
  - ← `oor`: `SendSubmitPackageRequest`, `SendFinalizePackageRequest`, `SendIncomingAckRequest`

## Invariants

- Ack watermark only advances AFTER durable local dispatch commit (prevents message loss on crash).
- Unary RPC responses use in-memory registry (no durability); on crash, callers retry with fresh correlation IDs.
- Idempotency keys are derived from message payload hash; same key on retry enables server deduplication.
- Ingress loop checkpoints pull cursor and ack state; on restart, resumes from checkpoint.

## Deep Docs

- [serverconn/README.md](README.md) — Architecture, usage guide, crash recovery paths.
- [docs/mailbox_architecture.md](../docs/mailbox_architecture.md) — Three-layer mailbox system.
- [docs/durable_actor_architecture.md](../docs/durable_actor_architecture.md) — Durable actor internals.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
