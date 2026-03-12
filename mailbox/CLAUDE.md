# mailbox

## Purpose

Durable envelope store and delivery primitives for the server-side mailbox
system. Provides persistent storage for envelopes exchanged between the server
and its clients.

## Key Types

- `Store` — Durable envelope persistence interface.
- `Envelope` — Unit of mailbox transport (wraps RPC request/response/event).
- `MemoryStore` — In-memory envelope store for testing.

## Relationships

- **Depends on**: client submodule's `mailbox/pb` (envelope proto definitions).
- **Depended on by**: `clientconn` (envelope storage and delivery), `mailboxrpcserver` (gRPC service layer).

## Invariants

- Envelopes are persisted before delivery acknowledgment.
- Delivery is at-least-once; consumers must handle deduplication.
- Store operations must be safe for concurrent access.

## Deep Docs

- [docs/clientconn_architecture.md](../docs/clientconn_architecture.md) — Mailbox role in client connection lifecycle.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
