# cmd/merge-sql-schemas

## Purpose

Build-time tool that applies all `.up.sql` migration files from
`db/sqlc/migrations` and `db/actordelivery/migrations` to an in-memory SQLite
database, then dumps the resulting combined schema to
`db/sqlc/schemas/generated_schema.sql`. Used by the sqlc code-generation
pipeline to give sqlc a single flat schema file.

## Key Types

- `main()` — Opens an in-memory SQLite DB, applies both migration directories
  in lexicographic order via `applyMigrationDir`, queries `sqlite_master` for
  all tables/views/indexes, and writes the concatenated DDL to the output file.
- `applyMigrationDir(db, dir)` — Reads `.up.sql` files from `dir` in sorted
  order and executes each against `db`. Returns an error on the first failure.

## Relationships

- **Depends on**: `modernc.org/sqlite` (in-memory SQLite driver), standard
  library only.
- **Depended on by**: `make sqlc` Makefile target (run before `sqlc generate`
  so sqlc sees a merged schema).

## Invariants

- Migration files are applied in lexicographic order within each directory;
  cross-directory order is fixed: `db/sqlc/migrations` before
  `db/actordelivery/migrations`.
- Output file is always overwritten; the tool is idempotent given the same
  migration inputs.
- Never edit `db/sqlc/schemas/generated_schema.sql` by hand — regenerate via
  `make sqlc`.

## Deep Docs

- [db/CLAUDE.md](../../db/CLAUDE.md) — DB layer and migration conventions.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
