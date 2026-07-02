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
- `CompoundMailboxID(serverID, clientID)` — Joins server and client pubkey-derived IDs with a colon so both sides derive the same per-client wire address independently.
- `MailboxAuthDigest` / `MailboxAuthMessage` — BIP-340 tagged hash digest construction for mailbox auth signatures. Uses `chainhash.TaggedHash` with the `MailboxAuthTagStr` domain separator.
- `SignMailboxAuth` / `VerifyMailboxAuth` / `ParseMailboxPubKey` — Schnorr sign/verify helpers for pubkey-derived mailbox identity.
- `MailboxTLSBindDigest` / `MailboxTLSBindMessage` / `SignMailboxTLSBind` / `VerifyMailboxTLSBind` — Schnorr sign/verify helpers (`mailbox_auth.go`) binding a secp256k1 mailbox identity to the SubjectPublicKeyInfo of the P-256 TLS leaf cert observed on the connection (issue #448), using the disjoint `MailboxTLSBindTagStr` domain separator so a TLS-bind signature can never be replayed as a mailbox-auth signature.
- `AuthHeaderKey` / `TLSBindHeaderKey` — Envelope header keys (`x-mailbox-auth-sig`, `x-mailbox-tls-bind-sig`) for the Schnorr auth and TLS-binding signatures.
- `MailboxAuthSigner` / `NewAuthenticatedMailboxClient` (`mailbox_auth_rpc.go`) — Function type and `mailboxpb.MailboxServiceClient` wrapper that signs the recipient mailbox ID on every Send/Pull/AckUpTo and injects the result as the `AuthHeaderKey` outgoing-metadata header. A nil `MailboxAuthSigner` returns the wrapped client unchanged, for unauthenticated test transports. Used by `swapclientserver.Register` to authenticate the mailbox edge for the swap subsystem's `MailboxOutSwapEventReceiver`.
- `GenerateClientTLSCert` — Creates an ephemeral P-256 mTLS client cert with the secp256k1 identity pubkey hex as Subject CN. Returns error on nil key.
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
- **Depended on by**: `round` (outbound RPCs), `oor` (durable transport), `darepod` (wiring), `swapclientserver` (`MailboxAuthSigner`/`NewAuthenticatedMailboxClient`/`CompoundMailboxID`/`PubKeyMailboxID` to authenticate the swap mailbox edge), `sdk/swaps` (`CompoundMailboxID`, `PubKeyMailboxID`, `serverconn/mailboxpull`).
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
- `ServerConnectionActor` runs a background heartbeat goroutine
  (`DefaultHeartbeatInterval` = 30s) to keep the mailbox session alive.
  `StartIngress` also sends one immediate priming heartbeat before the first
  Pull, but only when `ConnectorConfig.AuthSignature` or `TLSBindSignature`
  is set — plain in-memory/unauthenticated runtimes keep the old
  first-message-triggers-registration behavior instead of paying the extra
  round trip. Authenticated daemon connections need the priming send so the
  server records the Schnorr/TLS binding before ingress starts pulling.
- Ingress handles header-only error responses (nil body) by routing them as errors rather than panicking on nil proto.
- `SendClientEventRequest.CorrelationKey()` always returns the correct
  per-key FIFO lane key regardless of whether the message was constructed
  fresh or decoded from TLV. The `cachedCorrelationKey` field is populated
  during `Decode` via `tlv.TlvType8` so restarts do not lose FIFO routing.

## Deep Docs

- [serverconn/README.md](README.md) — Architecture, usage guide, crash recovery paths.
- [docs/mailbox_architecture.md](../docs/mailbox_architecture.md) — Three-layer mailbox system.
- [docs/durable_actor_architecture.md](../docs/durable_actor_architecture.md) — Durable actor internals.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
