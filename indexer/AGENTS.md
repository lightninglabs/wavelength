# indexer

## Purpose

Wallet-scoped VTXO, round, and OOR event query service for connected clients.
Each client is authenticated as a `Principal` and can only query events relevant
to their wallet. Dispatched via the mailbox RPC pipeline like other services.

## Key Types

- `Operator` — RPC dispatcher factory that creates per-request handlers. Exposes `RegisterService` to host additional services (e.g., ArkService) on its internal `ServeMux`, and `ServiceDispatchers` to build `DispatcherMap` entries for any registered service.
- `Service` — Query service implementation (list VTXOs, rounds, OOR events).
  Supports `SetVTXOProofPolicy(operatorKey, exitDelay)` for owner-pubkey
  proof verification on receive script registration.
- `Principal` — Authenticated client context (mailbox ID, wallet scope).
- `lineageResolver` — Per-request resolver for authoritative VTXO lineage
  metadata (round ID, commitment tx, batch expiry, tree depth, chain depth,
  tree path). Handles both round-backed and virtual (OOR) VTXOs with
  checkpoint chain tracing and per-outpoint caching.
- `ScriptAuthorizer` — Interface for wallet-scope authorization of receive
  script operations.
- Event types (`IncomingOOREvent`, `IncomingVTXOEvent`) with `ServiceMethod()`
  routing metadata for client-side EventRouter dispatch.

## Relationships

- **Depends on**: `clientconn` (per-client dispatch), `db` (wallet-scoped queries), `rounds` (round event subscription).
- **Depended on by**: root `darepo` (wiring).
- **Messages to/from**:
  - Receives query requests <- `clientconn` (from clients).
  - Returns query results -> `clientconn` (to clients).

## Invariants

- All queries are scoped to the authenticated Principal's wallet.
- Indexer is read-only; it never mutates round, VTXO, or OOR state.
- Owner-pubkey proof: when a receive script proof carries an owner pubkey
  (TLV type 10), the server reconstructs the expected VTXO tapscript from
  `(ownerKey, operatorKey, exitDelay)` and verifies the pkScript matches.
  The signature is verified against the raw owner key, not the taproot
  output key. When absent, the direct-P2TR path is used.
- `ServiceMethod()` on indexer event messages returns `arkServiceName`
  (not `indexerServiceName`) to match client-side EventRouter routes.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
