# vtxo

## Purpose

VTXO (Virtual Transaction Output) locking, lifecycle tracking, and persistence.
Provides thread-safe mutual exclusion to prevent concurrent mutations across
rounds and OOR transfers.

## Key Types

- `Locker` — Thread-safe VTXO mutual exclusion for concurrent access.
- `LeaseLocker` — Time-bounded VTXO locks with automatic expiry.
- `Store` — VTXO record persistence interface.
- `Record` — VTXO metadata (outpoint, amount, status, tree path).
- `Status` — Lifecycle state: `live`, `in_flight`, `spent`.

## Relationships

- **Depends on**: no internal dependencies (leaf package).
- **Depended on by**: `rounds` (VTXO locking during rounds), `oor` (VTXO locking during transfers), `db` (persistence implementation), `indexer` (VTXO queries).

## Invariants

- A VTXO can only be locked by one round or OOR session at a time.
- Lease locks expire automatically; expired locks must not block new operations.
- Status transitions are one-directional: `live` -> `in_flight` -> `spent`.
- The Locker must be safe for concurrent goroutine access.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
