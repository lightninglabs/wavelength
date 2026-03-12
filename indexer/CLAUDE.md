# indexer

## Purpose

Wallet-scoped VTXO, round, and OOR event query service for connected clients.
Each client is authenticated as a `Principal` and can only query events relevant
to their wallet. Dispatched via the mailbox RPC pipeline like other services.

## Key Types

- `Operator` — RPC dispatcher factory that creates per-request handlers.
- `Service` — Query service implementation (list VTXOs, rounds, OOR events).
- `Principal` — Authenticated client context (mailbox ID, wallet scope).
- Event types for round, VTXO, and OOR state change notifications.

## Relationships

- **Depends on**: `clientconn` (per-client dispatch), `db` (wallet-scoped queries), `rounds` (round event subscription).
- **Depended on by**: root `darepo` (wiring).
- **Messages to/from**:
  - Receives query requests <- `clientconn` (from clients).
  - Returns query results -> `clientconn` (to clients).

## Invariants

- All queries are scoped to the authenticated Principal's wallet.
- Indexer is read-only; it never mutates round, VTXO, or OOR state.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
