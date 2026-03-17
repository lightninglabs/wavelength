# mailbox

## Purpose

Durable envelope store and delivery primitives for the server-side mailbox
system. Provides persistent storage for envelopes exchanged between the server
and its clients.

## Key Types

- `Store` — Durable envelope persistence interface (`Append`, `Pull`, `AckUpTo`).
- `Envelope` — Unit of mailbox transport (wraps RPC request/response/event).
- `MemoryStore` — In-memory envelope store for testing and production use.
- `StoreConfig` — Exported configuration for store behavior (poll interval, size limits, per-mailbox capacity).
- `StoreOption` — Functional options: `WithPullPollInterval`, `WithMaxEnvelopeBytes`, `WithMaxEnvelopesPerMailbox`, `WithLogger`.
- `LocalMailboxClient` — Exported in-process mailbox adapter that wraps a `Store` with `MailboxServiceClient` semantics, enabling the systest to reuse the same adapter pattern as the production server.

## Relationships

- **Depends on**: client submodule's `mailbox/pb` (envelope proto definitions).
- **Depended on by**: `clientconn` (envelope storage and delivery), `mailboxrpcserver` (gRPC service layer), `db` (`MailboxEnvelopeStore` implements `Store`), `systest` (uses `LocalMailboxClient` for in-process transport).

## Invariants

- Envelopes are persisted before delivery acknowledgment.
- Delivery is at-least-once; consumers must handle deduplication.
- Store operations must be safe for concurrent access.

## Deep Docs

- [docs/clientconn_architecture.md](../docs/clientconn_architecture.md) — Mailbox role in client connection lifecycle.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
