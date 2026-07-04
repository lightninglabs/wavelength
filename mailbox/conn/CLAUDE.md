# mailbox/conn

## Purpose

Reusable mailbox connector primitives shared by client-side and server-side
connector runtimes. Contains protocol-adjacent building blocks only: typed
identifiers, deterministic idempotency helpers, ack watermark state machine
encoding, and the in-memory response waiter registry for unary correlation
delivery.

## Key Types

- `CorrelationID` — Typed string linking a mailbox KIND_REQUEST to its
  KIND_RESPONSE. Used as the registry key in `ResponseRegistry`.
- `IdempotencyKey` — Typed string for server-side semantic deduplication
  across retries. Stable across retries; differs from `msg_id`.
- `AckState` — Four-cursor watermark state machine
  (`PullCursor`, `DispatchCommittedTo`, `AckTarget`, `AckCommittedTo`)
  that governs safe ack progression. Serialized to/from TLV for checkpoint
  persistence (checkpoint type `"AckState"`). Invariant:
  `AckCommittedTo <= DispatchCommittedTo`.
- `ResponseRegistry` — In-memory waiter registry for unary RPC correlation.
  Maps `CorrelationID` to `actor.Promise[*mailboxpb.Envelope]`. Supports
  three scenarios: waiter registered before response, response arrives
  before waiter (buffered), and stale cleanup via configurable TTL.
  `HasWaiter` reports (after pruning stale entries) whether a live waiter
  is registered, letting the ingress loop split fast-path delivery from
  the durable dispatch transaction. `FailAll` completes every registered
  waiter with a given error and clears the set, used when the connector
  transitions to a terminal incompatible state.
- `DeliveryResult` — Tri-state enum returned by `ResponseRegistry.DeliverResponse`:
  - `DeliveryWaiter` — Response completed an active in-memory waiter.
  - `DeliveryBuffered` — Response buffered; no waiter registered yet.
  - `DeliveryDropped` — Response could not be stored or delivered (nil
    envelope or proto clone failure).
  `DeliveryResult` implements `String()` for human-readable logging.
- `WrappedProto` — TLV bridging type that adapts a `proto.Message` for use
  as a `tlv.RecordT` field; marshals/unmarshals via `proto.Marshal` /
  `proto.Unmarshal`.
- `StableEventMsgID` / `StableEventIdempotencyKey` — Deterministic ID
  derivation from payload bytes (SHA-256, first 16 bytes, hex-encoded with
  `"evt-"` / `"idem-"` prefix). Used by durable query types in `serverconn`
  to auto-derive stable IDs when the caller leaves them empty.
- `DefaultResponseWaiterTTL` — Default 10-minute TTL for stale waiter and
  buffered response cleanup.
- `ErrWaiterExpired` / `ErrWaiterCancelled` — Sentinel errors signaled to
  blocked `AwaitRPC` callers when a waiter is pruned or explicitly removed.
- `StatusError` — Typed wrapper around a non-OK `mailboxpb.Status`, carrying
  the failing `Op` (e.g. `"Send"`, `"Pull"`, `"AckUpTo"`) and the verbatim
  `Status` payload. It is the shared status type for every client Send,
  Pull, and AckUpTo path, replacing previously duplicated per-caller status
  handling. `Code()`, `IsPermanentVersion()`, `SupportedMailboxVersions()`,
  and `SupportedArkVersions()` expose the structured fields without
  flattening them into a string.
- `StatusMailboxVersionUnsupported` / `StatusArkVersionUnsupported` /
  `StatusArkVersionMismatch` / `StatusUpgradeRequired` — The four permanent,
  non-retryable mailbox status codes; the single source of truth consulted
  by `StatusError.IsPermanentVersion()` and `IsPermanentVersionError()`.
- `IsPermanentVersionError` — Free helper that unwraps (via `errors.As`) any
  error to check for a `StatusError` carrying a permanent version code.
  Durable senders use this to stop retrying and dead-letter the message.

## Relationships

- **Depends on**: `baselib/actor` (Promise/Future types), `mailbox/pb`
  (Envelope proto, `Status`), `lnd/tlv`.
- **Depended on by**: `serverconn` (uses `AckState`, `ResponseRegistry`,
  `WrappedProto`, `StableEventMsgID`/`StableEventIdempotencyKey`,
  `CorrelationID`/`IdempotencyKey`, `StatusError`); `darepod` (handles
  `*mailboxconn.StatusError` in operator/version-negotiation compatibility
  paths).

## Invariants

- `msg_id` differs on each retry; `idempotency_key` is stable across
  retries for server deduplication.
- `AckState` cursors are monotonic and must not decrease during normal
  operation. `AdvanceDispatch` and `AdvanceAck` enforce this.
- `ResponseRegistry.DeliverResponse` buffers at most one response per
  `CorrelationID`; duplicate arrivals before a waiter are ignored (returns
  `DeliveryBuffered` without overwriting).
- Stale waiters are completed with `ErrWaiterExpired` (not silently dropped)
  so blocked callers wake up with a clear error rather than hanging
  indefinitely.
- `WrappedProto` callers must use `tlv.NewRecordT` to assign the real TLV
  type; calling `Record()` directly yields type 0 and would silently conflict
  with other type-0 records.
- Permanent-version classification lives solely in `permanentVersionCodes`;
  unary, durable event, heartbeat, pull, and ack paths must all consult
  `StatusError.IsPermanentVersion()` / `IsPermanentVersionError()` rather than
  re-deriving their own code lists, so they stay in agreement.

## Deep Docs

- [mailbox/conn/doc.go](doc.go) — Package overview.
- [mailbox/CLAUDE.md](../CLAUDE.md) — Parent package overview.
- [docs/mailbox_architecture.md](../../docs/mailbox_architecture.md) — Full three-layer mailbox architecture.
- [docs/RPC_MAILBOX_CONTRACT.md](../../docs/RPC_MAILBOX_CONTRACT.md) — Envelope semantics and ack watermarks.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
