# mailboxrpcserver

## Purpose

gRPC mailbox service implementation exposing the mailbox store over gRPC for
client connections. Handles envelope send/receive RPCs and long-poll delivery.

## Key Types

- `Server` — gRPC service implementation wrapping the mailbox store.

## Relationships

- **Depends on**: `mailbox` (envelope store).
- **Depended on by**: root `darepo` (registered as gRPC service).

## Invariants

- Must enforce per-client isolation; clients can only access their own mailbox.
- Long-poll connections must be cleaned up on client disconnect.

## Deep Docs

- [docs/clientconn_architecture.md](../docs/clientconn_architecture.md) — Mailbox transport layer.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
