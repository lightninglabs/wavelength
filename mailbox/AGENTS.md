# mailbox

## Purpose

Mailbox protocol primitives across three sub-packages: wire format definitions
(pb), runtime interfaces for generated RPC stubs (rpc), and reusable connector
primitives for durable transport (conn).

## Sub-Packages

### mailbox/pb
- `Envelope` — Wire format: msg_id, idempotency_key, sender, recipient, body, rpc metadata, event_seq, headers.
- `RpcMeta` — RPC overlay with Kind enum (REQUEST, RESPONSE, EVENT) and correlation metadata.
- `MailboxServiceClient` — Edge API (Send, Pull, AckUpTo).

### mailbox/rpc
- `RPCClient` — Interface for generated stubs (SendRPC, AwaitRPC).
- `Router` / `ServeMux` / `HandlerFunc` — Server-side routing infrastructure.

### mailbox/conn
- `AckState` — Checkpoint-persisted watermark state (PullCursor, DispatchCommittedTo, AckTarget, AckCommittedTo).
- `ResponseRegistry` — In-memory waiter registry for unary RPC correlation.
- `WrappedProto` — TLV bridging for proto payload serialization.
- `CorrelationID` — Typed identifier for RPC request-response correlation.

## Relationships

- **Depends on**: `baselib/actor` (`conn` uses Promise/Future for response
  correlation).
- **Depended on by**: generated `*_mailboxrpc.pb.go` stubs across the repo
  (e.g. `arkrpc`, `oor`, `round`, `swaprpc`, `daemonrpc`) depend on
  `mailbox/rpc`'s runtime interfaces; `serverconn` and `darepod` depend on
  all three sub-packages to construct envelopes, route RPCs, and manage ack
  watermarks.

## Invariants

- `msg_id` differs on each retry; `idempotency_key` is stable across retries for server deduplication.
- `event_seq` is server-assigned monotonic ordering key; used as cursor for Pull/AckUpTo.
- Pull is long-poll with configurable timeout (default 5s).

## Deep Docs

- [mailbox/README.md](README.md) — Module structure guide.
- [docs/mailbox_architecture.md](../docs/mailbox_architecture.md) — Full three-layer architecture.
- [docs/RPC_MAILBOX_CONTRACT.md](../docs/RPC_MAILBOX_CONTRACT.md) — Envelope semantics and ack watermarks.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
