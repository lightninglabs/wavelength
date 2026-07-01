# serverconn

## Purpose

Unified connector for all mailbox traffic between client and remote Ark server,
combining durable egress (crash-safe events), low-latency unary RPCs, and
background ingress polling with event routing.

## Key Types

- `Runtime` — Main entry point wrapping DurableActor, ServerConnectionActor, and UnaryFacade. The egress DurableActor runs on the Read/Commit (`TxBehavior`) path: each handler builds its envelope and calls `Edge.Send` with NO SQLite writer held, then a short lease-fenced Commit folds the ack + dedup. It runs as a competing-consumer pool of `ConnectorConfig.EgressWorkers` worker loops, so the round and out-of-round actors' sends proceed concurrently; the single ingress puller is separate and unaffected. `MarkIncompatible(ctx, statusErr)` is the exported entry point for callers outside the connector (e.g. a refresh-only `GetInfo` on the daemon's direct ArkService channel) that detect a permanent version failure on a side channel. `StampEnvelope(env)` stamps the runtime-bound `MailboxProtocolVersion`/`ArkProtocolVersion` pair onto a locally constructed envelope (e.g. the darepod mailbox response path) before it is sent.
- `ServerConnectionActor` — Core behavior handling egress messages and the ingress loop. Dispatches `DurableUnaryQuery` values generically via `buildDurableUnary`. Wraps `cfg.Edge` in `newVersionStampingMailboxClient` at construction so every outbound `Send` is stamped with the bound version pair in one place, mirroring the auth decorator. Tracks compatibility state via `compatErr` (atomic pointer to `*mailboxconn.StatusError`, nil while COMPATIBLE), `compatOnce` (one-shot transition guard), and `ingressCancel` (cancel func for asynchronously stopping ingress/heartbeat on transition).
- `ArkVersionNegotiator` — Single home for Ark protocol version selection (`ark_version.go`). `Bootstrap` performs the one bootstrap `GetInfo` over the operator's **direct** ArkService connection (`ArkVersionGetInfoClient`, never the mailbox edge) and returns the response + selected version; the daemon parses domain terms from the same response. The free function `ValidateRefreshSelection(resp, boundVersion)` enforces that a refresh-only `GetInfo` keeps the runtime bound (returns a permanent `*StatusError` on drift/disable). Enabled versions are derived from the response's ACTIVE `ArkVersionPolicy` entries.
- `UnaryFacade` — Implements `mailboxrpc.RPCClient` for generated RPC stubs (low-latency path). Also provides `AwaitRPCTimeout` for bounded waits.
- `ConnectorConfig` — Wiring configuration (edge address, mailbox IDs, dispatchers, store, durable unary builder, `EgressWorkers`). `EgressWorkers` sizes the egress worker pool (default `DefaultEgressWorkers` = 4); `<= 1` keeps the legacy single sender. The `DurableUnaryBuilder` field must be set to handle `DurableUnaryQuery` message types; otherwise those messages are rejected. The `AuthSignature` field holds the Schnorr auth sig injected into every outbound envelope via `mergeAuthHeaders` (auth header always wins over caller-provided headers). `MailboxProtocolVersion` (replaces the old `ProtocolVersion` field) is the immutable mailbox transport version stamped on outbound envelopes — a stable code constant (`mailboxpb.MailboxProtocolVersionV1`), defaulted by `NewRuntime` when left zero. `ArkProtocolVersion` is the immutable Ark protocol version negotiated via `ArkVersionNegotiator.Bootstrap` and bound to the runtime for its lifetime; `NewRuntime` rejects a zero value. `OnIncompatible` is an optional non-blocking callback invoked exactly once (inline, on the transition) when the connector becomes permanently incompatible.
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
- `SendListVTXOsByScriptsRequest` — TLV-durable (type `2004`) indexer query message for phase-2 OOR metadata resolution. Persists PkScripts (count-prefixed, length-prefixed list), opaque AfterCursor, Limit, CorrelationID, MsgID, and IdempotencyKey; the proof-gated proto body is built by `DurableUnaryRequestBuilder.BuildListVTXOsByScriptsRequest`.
- `CorrelationKey()` on `SendClientEventRequest` — Forwards the inner
  `ServerMessage`'s per-key FIFO key. Uses a structural assertion on the
  inner message in the pre-Encode path; falls back to a `cachedCorrelationKey`
  (populated at TLV decode) in the post-Decode path, because `Decode`
  replaces the concrete inner message with a `rawServerMessage` that no
  longer implements `CorrelationKey()`. This ensures the durable mailbox
  enqueues events into the correct per-key FIFO lane (e.g. `oor/<session>`,
  `round/<id>`) even after a crash-replay decode cycle.
- `EventRouteConfig.ResolveKey` / `EnvelopeRouteConfig.ResolveKey` — Optional hook mapping an adapted inbound message to a more specific service key (e.g. a per-session durable mailbox) via `AddRoute`/`AddEnvelopeRoute`. When the resolved key has a live receptionist registration, the envelope is `Tell`'d straight to that actor, skipping the static `Key` hop; a miss (actor not yet spawned or reaped) falls back to `Key`, which retains admission ownership for actors that don't exist yet. Returning `false` always uses `Key`.

### Version-Compatibility Subsystem

Client and operator negotiate an immutable Ark protocol version once at
bootstrap and enforce it on every envelope thereafter:

- `ConnectorConfig.MailboxProtocolVersion` / `ArkProtocolVersion` — The
  immutable version pair bound to the runtime. `MailboxProtocolVersion`
  defaults to the code constant `mailboxpb.MailboxProtocolVersionV1` when
  left zero; `ArkProtocolVersion` comes from `ArkVersionNegotiator.Bootstrap`
  and `NewRuntime` refuses to construct a runtime with a zero value.
- `versionStampingMailboxClient` (`version_stamp.go`) — A `MailboxServiceClient`
  decorator, installed over `cfg.Edge` at `NewServerConnectionActor`
  construction, that overwrites `Envelope.ProtocolVersion` /
  `ArkProtocolVersion` on every outbound `Send` so no send path (including a
  replayed durable envelope) can carry a stale or caller-controlled version.
  `Pull`/`AckUpTo` carry no envelope and forward unchanged.
- `validateInboundEnvelope` (`inbound_version.go`) — Checked against every
  inbound envelope. Returns a permanent `*mailboxconn.StatusError`
  (`StatusMailboxVersionUnsupported` or `StatusArkVersionMismatch`) on any
  mismatch against the bound pair — there is no legacy fallback; an inbound
  Ark version of zero is a mismatch like any other.
- `edgeResponseError` (`compatibility.go`) — Centralizes the version check
  every edge Send/Pull/AckUpTo path would otherwise repeat: wraps transport
  errors, reports a nil response as a transport error, and converts a non-OK
  `mailboxpb.Status` into a `*mailboxconn.StatusError`.
- `ServerConnectionActor.checkPermanentStatus` — Inspects an edge-call error;
  if it unwraps to a permanent-version `*mailboxconn.StatusError`, drives
  `markIncompatible` and returns `true`. Transient/non-status errors return
  `false` so the existing retry policy still applies.
- `ServerConnectionActor.markIncompatible` — One-shot (via `compatOnce`)
  transition to the terminal INCOMPATIBLE state: caches the error in
  `compatErr`, cancels ingress/heartbeat asynchronously via `ingressCancel`
  (never joins — may run on the ingress goroutine itself), calls
  `responseRegistry.FailAll(err)` to unblock every in-flight unary waiter,
  and invokes `ConnectorConfig.OnIncompatible` exactly once. Every egress
  handler (`handleSendClientEvent`, `sendUnaryEnvelope`,
  `handleSendRPCRequest`) short-circuits via `compatibilityError()` before
  touching the edge once INCOMPATIBLE.
- `permanentAwareTellRetryPolicy` (`runtime.go`) — The durable actor's
  `TellRetryPolicy` for serverconn egress: dead-letters immediately (no
  retry) on `mailboxconn.IsPermanentVersionError`, otherwise defers to
  `actor.DefaultTellRetryPolicy`.
- `splitIngressEnvelopes` (`ingress.go`) — Partitions a pulled batch into
  in-memory-waiter responses vs. durable-dispatch envelopes ahead of
  `runFoldedDispatch`'s transaction. A `KIND_RESPONSE` takes the fast
  pre-transaction path only when `hasResponseWaiter` (backed by
  `ResponseRegistry.HasWaiter`) reports a live waiter for its correlation ID;
  everything else (unwaited responses, requests, events, malformed
  envelopes) folds into the transactional durable batch so its enqueue
  commits atomically with the cursor. `deliverWaiterResponses` delivers the
  waiter-backed split outside the transaction and returns "stragglers" —
  responses whose waiter vanished between the split peek and delivery — which
  `mergeEnvelopesByEventSeq` folds back into the durable batch in
  `event_seq` order so a durable enqueue never commits ahead of the cursor
  fold.

## Relationships

- **Depends on**: `baselib/actor` (DurableActor infrastructure), `mailbox/*` (Envelope, RpcMeta, MailboxServiceClient), `mailbox/conn` (`StatusError`, `IsPermanentVersionError`, `ResponseRegistry.HasWaiter`/`FailAll` for the version-compatibility subsystem), `arkrpc` (`GetInfo` request/response + `ArkVersionPolicy` for version negotiation).
- **Depended on by**: `round` (outbound RPCs), `oor` (durable transport), `darepod` (wiring).
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
- The COMPATIBLE→INCOMPATIBLE transition is one-shot and terminal: once
  `markIncompatible` runs, the connector never returns to COMPATIBLE for the
  life of the runtime. Every subsequent send observes the cached
  `*mailboxconn.StatusError` and short-circuits before contacting the edge.
- A `KIND_RESPONSE` envelope delivered on the ingress split's fast
  (pre-transaction) path is validated against the bound version pair before
  delivery, exactly like the durable dispatch path — a version mismatch on
  either partition must be able to drive the incompatibility transition.
- `splitIngressEnvelopes`'s waiter classification (`HasWaiter`) is a hint,
  not a guarantee: a waiter can vanish between the split peek and actual
  delivery (RPC deadline cancel or TTL prune). Any straggler MUST fold back
  into the durable transaction rather than being durably dispatched outside
  it, or its enqueue could commit ahead of the cursor fold.

## Deep Docs

- [serverconn/README.md](README.md) — Architecture, usage guide, crash recovery paths.
- [docs/mailbox_architecture.md](../docs/mailbox_architecture.md) — Three-layer mailbox system.
- [docs/durable_actor_architecture.md](../docs/durable_actor_architecture.md) — Durable actor internals.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
