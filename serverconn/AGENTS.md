# serverconn

## Purpose

Unified connector for all mailbox traffic between client and remote Ark server,
combining durable egress (crash-safe events), low-latency unary RPCs, and
background ingress polling with event routing.

## Key Types

- `Runtime` — Main entry point wrapping DurableActor, ServerConnectionActor, and UnaryFacade. The egress DurableActor runs on the Read/Commit (`TxBehavior`) path: each handler builds its envelope and calls `Edge.Send` with NO SQLite writer held, then a short lease-fenced Commit folds the ack + dedup. It runs as a competing-consumer pool of `ConnectorConfig.EgressWorkers` worker loops, so the round and out-of-round actors' sends proceed concurrently; the single ingress puller is separate and unaffected.
- `ServerConnectionActor` — Core behavior handling egress messages and the ingress loop. Dispatches `DurableUnaryQuery` values generically via `buildDurableUnary`.
- `ArkVersionNegotiator` — Single home for Ark protocol version selection (`ark_version.go`). `Bootstrap` performs the one bootstrap `GetInfo` over the operator's **direct** ArkService connection (`ArkVersionGetInfoClient`, never the mailbox edge) and returns the response + selected version; the daemon parses domain terms from the same response. The free function `ValidateRefreshSelection(resp, boundVersion)` enforces that a refresh-only `GetInfo` keeps the runtime bound (returns a permanent `*StatusError` on drift/disable). Enabled versions are derived from the response's ACTIVE `ArkVersionPolicy` entries.
- `UnaryFacade` — Implements `mailboxrpc.RPCClient` for generated RPC stubs (low-latency path). Also provides `AwaitRPCTimeout` for bounded waits.
- `ConnectorConfig` — Wiring configuration (edge address, mailbox IDs, dispatchers, store, durable unary builder, `EgressWorkers`). `EgressWorkers` sizes the egress worker pool (default `DefaultEgressWorkers` = 4); `<= 1` keeps the legacy single sender. The `DurableUnaryBuilder` field must be set to handle `DurableUnaryQuery` message types; otherwise those messages are rejected. The `AuthSignature` field holds the Schnorr auth sig injected into every outbound envelope via `mergeAuthHeaders` (auth header always wins over caller-provided headers).
- `PubKeyMailboxID` — Derives canonical mailbox ID from a public key (hex-encoded compressed SEC). Panics on nil.
- `MailboxAuthDigest` / `MailboxAuthMessage` — BIP-340 tagged hash digest construction for mailbox auth signatures. Uses `chainhash.TaggedHash` with the `MailboxAuthTagStr` domain separator.
- `SignMailboxAuth` / `VerifyMailboxAuth` / `ParseMailboxPubKey` — Schnorr sign/verify helpers for pubkey-derived mailbox identity.
- `AuthHeaderKey` — Envelope header key (`x-mailbox-auth-sig`) for the Schnorr auth signature.
- `GenerateClientTLSCert` — Creates an ephemeral P-256 mTLS client cert with the secp256k1 identity pubkey hex as Subject CN. Returns error on nil key.
- `EventRouter` — Registry mapping inbound `ServiceMethod`s to typed actor
  dispatch. `AddRoute`/`NewEventRoute` register durable actor-message routes;
  `AddEnvelopeRoute` registers raw-envelope handlers (e.g. shared RPC methods
  where a stale response is dropped via `ErrEnvelopeHandled` instead of
  delivered).
- `MailboxTLSBindDigest`/`Message`, `SignMailboxTLSBind`/`VerifyMailboxTLSBind`,
  `TLSBindHeaderKey` — Binds the ephemeral mTLS leaf cert's SPKI to the
  secp256k1 identity via a BIP-340 Schnorr signature, complementing
  `GenerateClientTLSCert` (the cert alone proves nothing; this signature
  proves the TLS key and the identity key are held by the same party).
- `NewAuthenticatedMailboxClient` — `mailboxpb.MailboxServiceClient` decorator
  that signs and attaches the `x-mailbox-auth-sig` header to every `Send`
  before forwarding to the wrapped edge transport.
- `AckState` — Four-cursor watermark state machine (PullCursor, DispatchCommittedTo, AckTarget, AckCommittedTo).
- `SendUnaryRequest` — Durable typed unary request that becomes a real unary RPC after commit. The response arrives via KIND_RESPONSE and, if no in-memory waiter exists, falls back to durable route dispatch via the EventRouter.
- `DurableUnaryRequestBuilder` — Interface for proof-gated request-body construction. Implementations build the actual proto request (e.g., with signed proofs) at send time, not at persist time. The interface is provided via `ConnectorConfig.DurableUnaryBuilder`.
- `DurableUnaryQuery` — Interface implemented by transport-native durable query messages that persist raw query parameters (not a full proto). The `ServerConnectionActor` matches any `DurableUnaryQuery` generically in its `Receive` loop and calls `buildDurableUnary` to construct a `SendUnaryRequest` on the fly, using `BuildBody`, `QueryCorrelationID`, `QueryMsgID`, `QueryIdempotencyKey`, and `ServiceMethod`.
- `SendListOORRecipientEventsByScriptRequest` — TLV-durable (type `2003`) indexer query message for phase-1 OOR receive resolution. Persists PkScript, AfterEventID, Limit, CorrelationID, MsgID, and IdempotencyKey; the proof-gated proto body is built at send time by `DurableUnaryRequestBuilder.BuildListOORRecipientEventsByScriptRequest`.
- `SendListVTXOsByScriptsRequest` — TLV-durable (type `2004`) indexer query message for phase-2 OOR metadata resolution. Persists PkScripts (count-prefixed, length-prefixed list), opaque AfterCursor, Limit, CorrelationID, MsgID, and IdempotencyKey; the proof-gated proto body is built by `DurableUnaryRequestBuilder.BuildListVTXOsByScriptsRequest`.
- `CorrelationKey()` on `SendClientEventRequest` — Forwards the inner
  `ServerMessage`'s per-key FIFO key. Uses a structural assertion on the
  inner message in the pre-Encode path; falls back to a `cachedCorrelationKey`
  (populated at TLV decode) in the post-Decode path, because `Decode`
  replaces the concrete inner message with a `rawServerMessage` that no
  longer implements `CorrelationKey()`. This ensures the durable mailbox
  enqueues events into the correct per-key FIFO lane (e.g. `oor/<session>`,
  `round/<id>`) even after a crash-replay decode cycle.

## Relationships

- **Depends on**: `baselib/actor` (DurableActor infrastructure), `mailbox/*` (Envelope, RpcMeta, MailboxServiceClient), `arkrpc` (`GetInfo` request/response + `ArkVersionPolicy` for version negotiation).
- **Depended on by**: `round` (outbound RPCs), `oor` (durable transport),
  `waved` (wiring), `sdk/swaps` (`CompoundMailboxID`, `PubKeyMailboxID`),
  `swapclientserver`.
- **Sends (egress → remote mailbox)**:
  - `SendClientEventRequest` (durable): wraps `JoinRoundRequest`, `JoinRoundAccept`, `JoinRoundReject`, `SubmitNoncesRequest`, `SubmitPartialSigRequest`, `SubmitForfeitSigRequest`. `JoinRoundAccept` / `JoinRoundReject` are the explicit responses to a server-issued seal-time `JoinRoundQuote` (#270); both echo the `quote_id` so the server can drop stale responses after a reseal.
  - `SendRPCRequest` (unary, non-durable): low-latency request-response RPCs
  - transport-native durable query messages for proof-gated indexer lookups
- **Routes (ingress → local actors via EventRouter)**:
  - → `round`: `CommitmentTxBuilt`, `JoinRoundQuoteReceived`, `NoncesAggregated`, `OperatorSigned`, `RoundJoined`, `BoardingFailed`. `JoinRoundQuoteReceived` is the seal-time fee quote (#270) routed by `RoundID`; the round actor buffers it via `bufferPendingQuote` when it arrives before the matching `RoundJoined` re-keys the FSM (the mailbox contract permits out-of-order delivery).
  - → `oor`: `SubmitAcceptedEvent`, `FinalizeAcceptedEvent`, `IncomingTransferEvent`
- **Receives (from local actors for outbound delivery)**:
  - ← `round`: `SendClientEventRequest` (outbox messages for persistence)
  - ← `oor`: `SendSubmitPackageRequest`, `SendFinalizePackageRequest`, `SendIncomingAckRequest`

## Invariants

- Ack watermark only advances AFTER durable local dispatch commit (prevents message loss on crash).
- Unary RPC responses use in-memory registry first; if no waiter exists (crash replay), the ingress falls back to durable EventRouter dispatch. The ResponseRegistry returns a tri-state delivery result (waiter/buffered/dropped) so the ingress knows whether to route durably.
- `SendClientEventRequest` auto-derives `Service`/`Method` from `Message.ServiceMethod()` when callers leave them empty, preventing silent drops.
- Idempotency keys are derived from message payload hash; same key on retry enables server deduplication.
- Egress is at-least-once: on the Read/Commit path the `Edge.Send` is not atomic with the mailbox ack (it never was, even on the old Classic path), so a crash or a lost lease between a successful send and its Commit redelivers and re-sends. The server absorbs the duplicate via the stable `MsgId`/`IdempotencyKey`. Under `EgressWorkers > 1` a `SendClientEventRequest` carries the inner message's `CorrelationKey`, so same-session events keep per-key FIFO order across the worker pool while distinct sessions send in parallel. `SendUnaryRequest` and `SendRPCRequest` are intentionally **unkeyed** (the `BaseMessage` default), so distinct unary/RPC sends may reorder across workers; that is safe only because each is an independent request/response RPC matched by an explicit correlation ID, not a position in an ordered stream. Any new order-sensitive egress message MUST define a `CorrelationKey`, or it will silently reorder under the pool.
- Ingress loop checkpoints pull cursor and ack state; on restart, resumes from checkpoint.
- `DurableUnaryQuery` values are handled generically in `ServerConnectionActor.Receive` via `buildDurableUnary`: the query is converted to a `SendUnaryRequest` using the configured `DurableUnaryRequestBuilder`. Adding a new durable indexer query type requires only implementing `DurableUnaryQuery` — no new `Receive` case is needed.
- `DurableUnaryQuery` implementations must produce stable identity bytes in `BuildBody` so that `MsgID` and `IdempotencyKey` are deterministic across restarts (auto-derived via `mailboxconn.StableEventMsgID` / `StableEventIdempotencyKey` when the caller leaves them empty).
- `ServerConnectionActor` runs a background heartbeat goroutine (`DefaultHeartbeatInterval` = 30s) to keep the mailbox session alive.
- Ingress handles header-only error responses (nil body) by routing them as errors rather than panicking on nil proto.
- `SendClientEventRequest.CorrelationKey()` always returns the correct
  per-key FIFO lane key regardless of whether the message was constructed
  fresh or decoded from TLV. The `cachedCorrelationKey` field is populated
  during `Decode` via `tlv.TlvType8` so restarts do not lose FIFO routing.
- Every outbound envelope is stamped with the runtime-bound mailbox
  transport and Ark protocol version pair (`stampEnvelopeVersions`/
  `versionStampingMailboxClient`), overwriting any caller-provided value.
  Every inbound envelope is checked against the same bound pair
  (`validateInboundEnvelope`); a mismatch is always a permanent
  `*mailboxconn.StatusError` — there is no legacy zero-version fallback,
  since client and operator are always deployed with a negotiated version.

## Deep Docs

- [serverconn/README.md](README.md) — Architecture, usage guide, crash recovery paths.
- [docs/mailbox_architecture.md](../docs/mailbox_architecture.md) — Three-layer mailbox system.
- [docs/mailbox_transport_serverconn_clientconn.md](../docs/mailbox_transport_serverconn_clientconn.md) — Transport split between serverconn (client-side) and clientconn (server-side).
- [docs/durable_actor_architecture.md](../docs/durable_actor_architecture.md) — Durable actor internals.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
