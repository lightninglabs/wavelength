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
- `Record` — Server-side VTXO record. Carries outpoint, value,
  `PolicyTemplate` (serialized arkscript policy, the preferred ownership
  source), pkScript, status, in-flight owner, and the optional legacy
  descriptor `(OwnerKey, OperatorKeyDesc, ExitDelay)`. Both DB-backed and
  in-memory stores round-trip the same record shape.
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
- **`MarkSpent` enforces authoritative ownership.** The caller must pass the
  `LockOwner` that holds the `in_flight` lock. Transition rules: only
  `in_flight(owner)` -> `spent` is accepted; `in_flight` held by a different
  owner returns an error; `live` -> `spent` is rejected (must lock first);
  `spent` -> `spent` is idempotent for any caller. This closes the window
  where a concurrent operation could mark a VTXO spent without holding the
  authoritative lock.
- `Record` descriptor metadata is all-or-nothing: if any of `OwnerKey`,
  `OperatorKeyDesc`, or `ExitDelay` is set, all three must be set and
  consistent. `ValidateDescriptorMetadata` is the canonical check and must
  be called on `Create`.
- `PolicyTemplate` is independently optional: it may be absent for
  registration-auth-only VTXOs, but when present it must match the
  existing record's template on duplicate `Create` (the in-memory store
  now checks `PolicyTemplate` equality in its duplicate detection, matching
  the DB-backed `InsertVTXOIfAbsent` path).
- Duplicate `Create` calls for the same outpoint must agree on `pkScript` and
  `PolicyTemplate` of the existing record. The legacy descriptor fields
  are no longer checked for equality in the production store.
- DB-backed and in-memory stores must round-trip the same record shape so
  OOR and rounds code can target the `vtxo.Store` interface without
  branching on backend.

## Deep Docs

- [docs/authoritative_locking.md](../docs/authoritative_locking.md) — Server-side locking model: ownership rules, FSM ordering, recovery invariants.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
