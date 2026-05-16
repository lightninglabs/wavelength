# sdk/swaps/sqlc

## Purpose

Generated sqlc query bindings for the swap client's isolated SQLite database.
Do not edit — regenerate via `make sqlc`.

## Key Types

- `Querier` — Generated interface for swap-session persistence operations
  (insert/update/get/list swap sessions).
- Generated row structs — Map SQL columns to typed Go fields for swap session
  records.

## Relationships

- **Depends on**: nothing beyond standard `database/sql`.
- **Depended on by**: `sdk/swaps` (the `Store` type wraps these generated
  queries with migration and higher-level session helpers).

## Invariants

- All files are generated. Never edit them manually.
- Regenerate via `make sqlc` after changing swap-related query or schema files.

## Deep Docs

- [ARCHITECTURE.md](../../../ARCHITECTURE.md) — System-wide package map.
