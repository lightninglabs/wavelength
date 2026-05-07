# db/sqlc

## Purpose

sqlc-generated type-safe query layer for the main darepo-client schema. All
`.sql.go` files are generated from `db/queries/` — do not edit them manually.
`db_custom.go` is the only hand-written file; it extends the generated layer
with a `BackendType` enum and a `Backend()` method so callers can distinguish
SQLite from Postgres at runtime.

## Key Types

- `Querier` — Generated interface listing every SQL query (50+ methods
  across boarding, rounds, VTXOs, OOR artifacts, ledger entries, UTXO audit
  log, chain info, and unilateral exit jobs). Production callers receive a
  concrete `*Queries` via `New(DBTX)`.
- `Queries` — Concrete struct implementing `Querier`. Holds a `DBTX` (the
  `*sql.DB` or `*sql.Tx` passed to `New`).
- `BackendType` — Enum (`BackendTypeSqlite`, `BackendTypePostgres`,
  `BackendTypeUnknown`) stored on a `wrappedTX` so callers can adapt behavior
  to the active backend without separate configuration plumbing.
- `Queries.Backend()` — Returns the `BackendType` of the underlying database
  handle. Returns `BackendTypeUnknown` if the handle was not wrapped via the
  standard factory.

## Relationships

- **Depends on**: `database/sql` (DBTX interface).
- **Depended on by**: `db` (parent package uses `Querier` for all SQL
  operations), `db/migrate` (schema migration management).

## Invariants

- All `.sql.go` files are generated. Regenerate with `make sqlc`.
- `db_custom.go` is the only file safe to edit; keep it minimal.
- `Backend()` returns `BackendTypeUnknown` only if the standard factory was
  bypassed — production code never reaches that branch.

## Deep Docs

- [db/CLAUDE.md](../CLAUDE.md) — Parent db package overview.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
