# sdk/swaps/sqlc

## Purpose

sqlc-generated type-safe query layer for the SDK swap-session schema. All
files are generated — do not edit manually. Regenerate with `make sqlc`.

## Key Types

- `Querier` — Generated interface for swap session SQL operations:
  insert/get/list swap sessions.
- `Queries` — Concrete struct implementing `Querier`.

## Relationships

- **Depends on**: `database/sql`.
- **Depended on by**: `sdk/swaps` (uses `Querier` for swap session
  persistence).

## Invariants

- All files are generated. Do not edit manually.
- Schema is managed by `sdk/swaps` independently of the main client schema.

## Deep Docs

- [sdk/swaps/CLAUDE.md](../CLAUDE.md) — Parent package overview.
- [ARCHITECTURE.md](../../../ARCHITECTURE.md) — System-wide package map.
