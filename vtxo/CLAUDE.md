# vtxo

## Purpose

VTXO (Virtual Transaction Output) locking, lifecycle tracking, and persistence.
Provides thread-safe mutual exclusion to prevent concurrent mutations across
rounds and OOR transfers, and a record shape shared by both the in-memory and
DB-backed stores.

## Key Types

- `Locker` — Thread-safe VTXO mutual exclusion for concurrent access.
- `LeaseLocker` — Time-bounded VTXO locks with automatic expiry.
- `Store` — VTXO record persistence interface. `InMemoryStore` is the
  reference implementation for tests; `db.VTXOStoreDB` is the production
  implementation that enriches receive-script-backed records at
  materialization time via `db/receive_script_vtxo_metadata.go`.
- `Record` — Server-side VTXO record. Carries outpoint, value, pkScript,
  status, in-flight owner, and the optional collaborative descriptor
  `(OwnerKey, OperatorKeyDesc, ExitDelay)`. A single operator key
  descriptor is the source of truth; both DB-backed and in-memory stores
  round-trip the same record shape.
- `Status` — Lifecycle state: `live`, `in_flight`, `spent`.
- `ValidateDescriptorMetadata` — Validates that a record either has no
  descriptor metadata or carries a complete, consistent
  `(OwnerKey, OperatorKeyDesc with PubKey, ExitDelay > 0)` tuple.

## Relationships

- **Depends on**: no internal dependencies (leaf package).
- **Depended on by**: `rounds` (VTXO locking during rounds), `oor` (VTXO
  locking and record materialization during transfers), `db` (persistence
  implementation and receive-script metadata enrichment), `indexer` (VTXO
  queries).

## Invariants

- A VTXO can only be locked by one round or OOR session at a time.
- Lease locks expire automatically; expired locks must not block new
  operations.
- Status transitions are one-directional: `live` -> `in_flight` -> `spent`.
- The Locker must be safe for concurrent goroutine access.
- `Record` descriptor metadata is all-or-nothing: if any of `OwnerKey`,
  `OperatorKeyDesc`, or `ExitDelay` is set, all three must be set and
  consistent. `ValidateDescriptorMetadata` is the canonical check and must
  be called on `Create`.
- Duplicate `Create` calls for the same outpoint must agree on the owner
  key, operator key descriptor, and exit delay of the existing record.
  This keeps postgres-safe upserts (`InsertVTXOIfAbsent`) consistent with
  the in-memory store.
- DB-backed and in-memory stores must round-trip the same record shape so
  OOR and rounds code can target the `vtxo.Store` interface without
  branching on backend.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
