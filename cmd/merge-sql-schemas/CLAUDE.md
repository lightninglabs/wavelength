# cmd/merge-sql-schemas

## Purpose

Utility that reads migration files from `db/sqlc/migrations/`, executes them on
an in-memory SQLite database, and outputs the consolidated schema to
`db/sqlc/schemas/generated_schema.sql`.

## Relationships

- **Depends on**: `db` (migration files).
- **Depended on by**: `make sqlc` (schema generation pipeline).
