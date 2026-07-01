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
  `HasWaiter(id)` reports (after pruning stale entries) whether a live
  waiter is registered — used by the ingress loop to decide, per
  `KIND_RESPONSE`, whether it can take the fast pre-transaction delivery
  path or must fold into the durable dispatch transaction so the enqueue
  commits atomically with the cursor. `FailAll(err)` completes every
  registered waiter's promise with `err` and clears the waiter set,
  used when the connector transitions to a terminal incompatible state so
  no caller blocks on a response that will never arrive.
- `DeliveryResult` — Tri-state enum returned by `ResponseRegistry.DeliverResponse`:
  - `DeliveryWaiter` — Response completed an active in-memory waiter.
  - `DeliveryBuffered` — Response buffered; no waiter registered yet.
  - `DeliveryDropped` — Response could not be stored or delivered (nil
    envelope or proto clone failure).
  `DeliveryResult` implements `String()` for human-readable logging.
- `StatusError` — Typed error wrapping a non-OK `mailboxpb.Status` from a
  Send/Pull/AckUpTo edge call, replacing ad hoc string errors so callers can
  recover the full structured payload (`Code()`, `SupportedMailboxVersions()`,
  `SupportedArkVersions()`). `IsPermanentVersion()` reports whether the code
  is one of the four permanent version-compatibility codes
  (`StatusMailboxVersionUnsupported`, `StatusArkVersionUnsupported`,
  `StatusArkVersionMismatch`, `StatusUpgradeRequired`). The free function
  `IsPermanentVersionError(err)` unwraps (`errors.As`) to classify any
  wrapped error, letting durable senders stop retrying and dead-letter the
  message instead of treating a version failure as transient.
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

## Relationships

- **Depends on**: `baselib/actor` (Promise/Future types), `mailbox/pb`
  (Envelope proto), `lnd/tlv`.
- **Depended on by**: `serverconn` (uses `AckState`, `ResponseRegistry`,
  `WrappedProto`, `StableEventMsgID`/`StableEventIdempotencyKey`,
  `CorrelationID`/`IdempotencyKey`, `StatusError`/`IsPermanentVersionError`
  for its version-compatibility subsystem).

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
- `HasWaiter` prunes stale entries before answering, so an expired waiter
  never masquerades as live and misroutes a response onto the fast
  (non-durable) delivery path.
- Permanent version-compatibility codes (see `StatusError.IsPermanentVersion`)
  must never be retried by durable senders — `IsPermanentVersionError` is the
  single source of truth callers use to distinguish them from transient
  transport/internal failures.
- `WrappedProto` callers must use `tlv.NewRecordT` to assign the real TLV
  type; calling `Record()` directly yields type 0 and would silently conflict
  with other type-0 records.

## Deep Docs

- [mailbox/conn/doc.go](doc.go) — Package overview.
- [mailbox/CLAUDE.md](../CLAUDE.md) — Parent package overview.
- [docs/mailbox_architecture.md](../../docs/mailbox_architecture.md) — Full three-layer mailbox architecture.
- [docs/RPC_MAILBOX_CONTRACT.md](../../docs/RPC_MAILBOX_CONTRACT.md) — Envelope semantics and ack watermarks.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
