# serverconn

## Purpose

Unified connector for all mailbox traffic between client and remote Ark server,
combining durable egress (crash-safe events), low-latency unary RPCs, and
background ingress polling with event routing.

## Key Types

- `Runtime` — Main entry point wrapping DurableActor, ServerConnectionActor, and UnaryFacade. The egress DurableActor runs on the Read/Commit (`TxBehavior`) path: each handler builds its envelope and calls `Edge.Send` with NO SQLite writer held, then a short lease-fenced Commit folds the ack + dedup. It runs as a competing-consumer pool of `ConnectorConfig.EgressWorkers` worker loops, so the round and out-of-round actors' sends proceed concurrently; the single ingress puller is separate and unaffected.
- `ServerConnectionActor` — Core behavior handling egress messages and the ingress loop. Dispatches `DurableUnaryQuery` values generically via `buildDurableUnary`.
- `ArkVersionNegotiator` — Single home for Ark protocol version selection (`ark_version.go`). `Bootstrap` performs the one bootstrap `GetInfo` over the operator's **direct** ArkService connection (`ArkVersionGetInfoClient`, never the mailbox edge) and returns the response + selected version; the daemon parses domain terms from the same response. The free function `ValidateRefreshSelection(resp, boundVersion)` enforces that a refresh-only `GetInfo` keeps the runtime bound (returns a permanent `*StatusError` on drift/disable). Enabled versions are derived from the response's ACTIVE `ArkVersionPolicy` entries.
- `UnaryFacade` — Implements `mailboxrpc.RPCClient` for generated RPC stubs (low-latency path). Bounded waits come from the caller's context plus the response registry TTL; there is no separate timeout entry point. `SendRPC` gates on `ConnectorConfig.MaxInFlightUnary` and fails a send that would exceed it with `codes.ResourceExhausted`.
- `ConnectorConfig` — Wiring configuration (edge address, mailbox IDs, dispatchers, store, durable unary builder, `EgressWorkers`, `MaxInFlightUnary`). `EgressWorkers` sizes the egress worker pool (default `DefaultEgressWorkers` = 4); `<= 1` keeps the legacy single sender. The `DurableUnaryBuilder` field must be set to handle `DurableUnaryQuery` message types; otherwise those messages are rejected. The `AuthSignature` field holds the Schnorr auth sig injected into every outbound envelope via `mergeAuthHeaders` (auth header always wins over caller-provided headers).
- `PubKeyMailboxID` — Derives canonical mailbox ID from a public key (hex-encoded compressed SEC). Panics on nil.
- `MailboxAuthDigest` / `MailboxAuthMessage` — BIP-340 tagged hash digest construction for mailbox auth signatures. Uses `chainhash.TaggedHash` with the `MailboxAuthTagStr` domain separator over `senderCompressedPubKey || recipientMailboxID`. **Do not read this as preventing cross-server replay on its own** — see the invariant below for which callers get that property and which do not.
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
- `NewAuthenticatedMailboxClient` / `MailboxAuthSigner` — `mailboxpb.MailboxServiceClient`
  decorator that signs and attaches the `x-mailbox-auth-sig` header to every
  `Send`, `Pull`, and `AckUpTo` before forwarding to the wrapped edge
  transport. `MailboxAuthSigner` is `func(ctx, recipientMailboxID) (string,
  error)` returning the hex-encoded signature; `waved` supplies a memoizing
  implementation. Wrapping unconditionally means the daemon works against an
  operator that terminates TLS at a proxy (and so never sees a client
  certificate) without that posture leaking into client config.
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

- **Mailbox auth cross-server replay resistance is per-caller, not universal.**
  Including the recipient mailbox ID in the digest binds the signature to what
  it addresses, but only `Send` gets the strong version: it passes the
  compound `operator:client` recipient, which embeds the operator's
  pubkey-derived ID, so a `Send` signature is useless at any other operator.
  `Pull` and `AckUpTo` pass the client's own **plain** mailbox ID (from
  `ConnectorConfig.LocalMailboxID`), which carries no operator component — so
  that digest is identical at every operator, and one `Pull`/`Ack` signature
  authorizes that client's mailbox at every operator its identity key is known
  to. Identity keys are a deterministic derivation, so a wallet driving two
  operators presents the same credential to both. Closing the `Pull`/`Ack`
  case means folding a server identity into the digest, which is a wire change
  on both sides.
- Ack watermark only advances AFTER durable local dispatch commit (prevents message loss on crash).
- The ingress fold never holds the database writer across network IO. `runFoldedDispatch` runs waiter-backed responses and the `ConnectorConfig.NonTxRoutes` requests BEFORE opening the write transaction; only durable enqueues and the cursor checkpoint go inside it. A route is hoisted only when it is listed in `NonTxRoutes` AND the envelope is a `KIND_REQUEST`, so a durable actor `Tell` can never escape the fold. An envelope of any other kind arriving on a marked route is skip-warned by `dispatchBatch` rather than dispatched, because the mux bridge ignores `env.Rpc.Kind` and would otherwise serve a sender-mislabeled envelope over the network with the writer held. Any new dispatcher that terminates in `Edge.Send` rather than a durable enqueue MUST be added to `NonTxRoutes` at wiring time (see `waved.Server.buildRPCDispatchers`), otherwise it pins the SQLite global writer lock (production opens with `_txlock=immediate`) or a SERIALIZABLE Postgres snapshot for the length of a round trip to the operator.
- Pre-transaction dispatch happens before the commit, never after. A crash in between re-pulls the batch and redelivers, which is the at-least-once contract; committing first would advance the cursor past a request that was never answered.
- Unary RPC responses use in-memory registry first; if no waiter exists (crash replay), the ingress falls back to durable EventRouter dispatch. The ResponseRegistry returns a tri-state delivery result (waiter/buffered/dropped) so the ingress knows whether to route durably.
- `SendClientEventRequest` auto-derives `Service`/`Method` from `Message.ServiceMethod()` when callers leave them empty, preventing silent drops.
- Idempotency keys are derived from message payload hash; same key on retry enables server deduplication.
- Egress is at-least-once: on the Read/Commit path the `Edge.Send` is not atomic with the mailbox ack (it never was, even on the old Classic path), so a crash or a lost lease between a successful send and its Commit redelivers and re-sends. The server absorbs the duplicate via the stable `MsgId`/`IdempotencyKey`. Under `EgressWorkers > 1` a `SendClientEventRequest` carries the inner message's `CorrelationKey`, so same-session events keep per-key FIFO order across the worker pool while distinct sessions send in parallel. `SendUnaryRequest` and `SendRPCRequest` are intentionally **unkeyed** (the `BaseMessage` default), so distinct unary/RPC sends may reorder across workers; that is safe only because each is an independent request/response RPC matched by an explicit correlation ID, not a position in an ordered stream. Any new order-sensitive egress message MUST define a `CorrelationKey`, or it will silently reorder under the pool.
- The mailbox protocol has no cancel envelope kind (`RpcMeta_Kind` is
  REQUEST/RESPONSE/EVENT only), so a unary caller that gives up on its deadline
  cannot recall the request: the operator runs it to completion and the
  response arrives with no waiter, falling through to durable route dispatch.
  `ConnectorConfig.MaxInFlightUnary` (default `DefaultMaxInFlightUnary` = 256)
  is the only client-side bound on how much of that abandoned work one client
  can queue. The cap is soft: concurrent senders may overshoot it by one burst,
  since making it exact would mean holding the registry lock across the send.
- A unary caller that re-issues must own its idempotency key for the life of
  the logical request (see `mailboxrpc.Retry`). `SendRPC` mints a fresh key
  only when the caller leaves `RPCOptions.IdempotencyKey` empty, which is
  correct for a single-shot call and defeats deduplication for a retry.
- Ingress loop checkpoints pull cursor and ack state; on restart, resumes from checkpoint.
- The ingress loop keeps **two** failure counters and they are not
  interchangeable. `failCount` drives the shared exponential backoff across
  every transport and checkpoint failure; `pullFailCount` is pull-only and
  exists solely to scope alerting to one remote-pull outage. Only the first
  failure of an episode logs `Pull failed, retrying` at `Warn` (so a real
  dependency outage stays visible); later attempts log at `Debug` with a
  `consecutive_failures` count, and a successful pull resets the counter so a
  later outage warns again. Reusing the shared `failCount` here would let an
  unrelated backoff suppress the first alert of a genuine pull outage.
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
- [p-models/durableactor/README.md](../p-models/durableactor/README.md) — P models of the ingress cursor fold and of the dispatch deferral/redrive against bounded in-memory mailboxes.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
