# serverconn

## Purpose

Unified connector for all mailbox traffic between client and remote Ark server,
combining durable egress (crash-safe events), low-latency unary RPCs, and
background ingress polling with event routing.

## Key Types

- `Runtime` — Main entry point wrapping DurableActor, ServerConnectionActor, and UnaryFacade.
- `ServerConnectionActor` — Core behavior handling egress messages and the ingress loop. Dispatches `DurableUnaryQuery` values generically via `buildDurableUnary`.
- `UnaryFacade` — Implements `mailboxrpc.RPCClient` for generated RPC stubs (low-latency path). Also provides `AwaitRPCTimeout` for bounded waits.
- `ConnectorConfig` — Wiring configuration (edge address, mailbox IDs, dispatchers, store, durable unary builder). The `DurableUnaryBuilder` field must be set to handle `DurableUnaryQuery` message types; otherwise those messages are rejected. The `AuthSignature` field holds the Schnorr auth sig injected into every outbound envelope via `mergeAuthHeaders` (auth header always wins over caller-provided headers).
- `PubKeyMailboxID` — Derives canonical mailbox ID from a public key (hex-encoded compressed SEC). Panics on nil.
- `MailboxAuthDigest` / `MailboxAuthMessage` — BIP-340 tagged hash digest construction for mailbox auth signatures. Uses `chainhash.TaggedHash` with the `MailboxAuthTagStr` domain separator.
- `SignMailboxAuth` / `VerifyMailboxAuth` / `ParseMailboxPubKey` — Schnorr sign/verify helpers for pubkey-derived mailbox identity.
- `AuthHeaderKey` — Envelope header key (`x-mailbox-auth-sig`) for the Schnorr auth signature.
- `GenerateClientTLSCert` — Creates an ephemeral P-256 mTLS client cert with the secp256k1 identity pubkey hex as Subject CN. Returns error on nil key.
- `AckState` — Four-cursor watermark state machine (PullCursor, DispatchCommittedTo, AckTarget, AckCommittedTo).
- `SendUnaryRequest` — Durable typed unary request that becomes a real unary RPC after commit. The response arrives via KIND_RESPONSE and, if no in-memory waiter exists, falls back to durable route dispatch via the EventRouter.
- `DurableUnaryRequestBuilder` — Interface for proof-gated request-body construction. Implementations build the actual proto request (e.g., with signed proofs) at send time, not at persist time. The interface is provided via `ConnectorConfig.DurableUnaryBuilder`.
- `DurableUnaryQuery` — Interface implemented by transport-native durable query messages that persist raw query parameters (not a full proto). The `ServerConnectionActor` matches any `DurableUnaryQuery` generically in its `Receive` loop and calls `buildDurableUnary` to construct a `SendUnaryRequest` on the fly, using `BuildBody`, `QueryCorrelationID`, `QueryMsgID`, `QueryIdempotencyKey`, and `ServiceMethod`.
- `SendListOORRecipientEventsByScriptRequest` — TLV-durable (type `2003`) indexer query message for phase-1 OOR receive resolution. Persists PkScript, AfterEventID, Limit, CorrelationID, MsgID, and IdempotencyKey; the proof-gated proto body is built at send time by `DurableUnaryRequestBuilder.BuildListOORRecipientEventsByScriptRequest`.
- `SendListVTXOsByScriptsRequest` — TLV-durable (type `2004`) indexer query message for phase-2 OOR metadata resolution. Persists PkScripts (count-prefixed, length-prefixed list), AfterCursor, Limit, CorrelationID, MsgID, and IdempotencyKey; the proof-gated proto body is built by `DurableUnaryRequestBuilder.BuildListVTXOsByScriptsRequest`.

## Relationships

- **Depends on**: `baselib/actor` (DurableActor infrastructure), `mailbox/*` (Envelope, RpcMeta, MailboxServiceClient).
- **Depended on by**: `round` (outbound RPCs), `oor` (durable transport), `darepod` (wiring).
- **Sends (egress → remote mailbox)**:
  - `SendClientEventRequest` (durable): wraps `JoinRoundRequest`, `SubmitNoncesRequest`, `SubmitPartialSigRequest`, `SubmitForfeitSigRequest`
  - `SendRPCRequest` (unary, non-durable): low-latency request-response RPCs
  - transport-native durable query messages for proof-gated indexer lookups
- **Routes (ingress → local actors via EventRouter)**:
  - → `round`: `CommitmentTxBuilt`, `NoncesAggregated`, `OperatorSigned`, `RoundJoined`, `BoardingFailed`
  - → `oor`: `SubmitAcceptedEvent`, `FinalizeAcceptedEvent`, `IncomingTransferEvent`
- **Receives (from local actors for outbound delivery)**:
  - ← `round`: `SendClientEventRequest` (outbox messages for persistence)
  - ← `oor`: `SendSubmitPackageRequest`, `SendFinalizePackageRequest`, `SendIncomingAckRequest`

## Invariants

- Ack watermark only advances AFTER durable local dispatch commit (prevents message loss on crash).
- Unary RPC responses use in-memory registry first; if no waiter exists (crash replay), the ingress falls back to durable EventRouter dispatch. The ResponseRegistry returns a tri-state delivery result (waiter/buffered/dropped) so the ingress knows whether to route durably.
- `SendClientEventRequest` auto-derives `Service`/`Method` from `Message.ServiceMethod()` when callers leave them empty, preventing silent drops.
- Idempotency keys are derived from message payload hash; same key on retry enables server deduplication.
- Ingress loop checkpoints pull cursor and ack state; on restart, resumes from checkpoint.
- `DurableUnaryQuery` values are handled generically in `ServerConnectionActor.Receive` via `buildDurableUnary`: the query is converted to a `SendUnaryRequest` using the configured `DurableUnaryRequestBuilder`. Adding a new durable indexer query type requires only implementing `DurableUnaryQuery` — no new `Receive` case is needed.
- `DurableUnaryQuery` implementations must produce stable identity bytes in `BuildBody` so that `MsgID` and `IdempotencyKey` are deterministic across restarts (auto-derived via `mailboxconn.StableEventMsgID` / `StableEventIdempotencyKey` when the caller leaves them empty).
- `ServerConnectionActor` runs a background heartbeat goroutine (`DefaultHeartbeatInterval` = 30s) to keep the mailbox session alive.
- Ingress handles header-only error responses (nil body) by routing them as errors rather than panicking on nil proto.

## Deep Docs

- [serverconn/README.md](README.md) — Architecture, usage guide, crash recovery paths.
- [docs/mailbox_architecture.md](../docs/mailbox_architecture.md) — Three-layer mailbox system.
- [docs/durable_actor_architecture.md](../docs/durable_actor_architecture.md) — Durable actor internals.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
