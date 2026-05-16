# db/sqlc

## Purpose

Generated sqlc query bindings for the main daemon database schema. Do not
edit — regenerate via `make sqlc`.

## Key Types

- `Querier` — Generated interface for all domain SQL queries: boarding,
  chain info, fee accounting, OOR artifacts, rounds, unilateral exits, VTXOs,
  and UTXO audit log.
- Domain row structs (`BoardingAddress`, `Round`, `Vtxo`, `OorArtifact`,
  `FeeAccountingEntry`, etc.) — Generated structs mapping SQL columns to Go
  types.

## Relationships

- **Depends on**: nothing beyond standard `database/sql`.
- **Depended on by**: `db` (the `Store` wrapper that adds migration,
  transaction helpers, and higher-level composite operations on top of
  the generated queries).

## Invariants

- All files in this directory are generated. Never edit them manually.
- Regenerate by running `make sqlc` after changing `db/queries/` or
  `db/schema/`.

## Deep Docs

- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
