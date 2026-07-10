# db/actordelivery/migrations

## Purpose

Migration runner for the actor-delivery sub-schema. Applies embedded SQL
migration files (via `//go:embed`) to create and evolve the
`actor_delivery_schema_migrations` table and its dependent objects using the
generic `db/migrate` orchestration layer.

## Key Types

- `Config` — Migration configuration: `MigrationsTable` (default
  `"actor_delivery_schema_migrations"`), `DatabaseName` (default
  `"actor_delivery"`), `LatestVersion` (downgrade guard, default
  `LatestMigrationVersion`), optional `Log btclog.Logger`.
- `LatestMigrationVersion = 1` — Current schema version; bump when adding a
  new SQL migration file. The single `000001_durable_mailbox` migration
  already includes the nullable `correlation_key` column on
  `mailbox_messages` and the filtered composite index that backs the
  per-correlation-key FIFO anti-join in `LeaseNextMailboxMessage`.
- `RunMigrations(db, backend, cfg)` — Applies actor-delivery migrations.
  Validates inputs, applies `Config` defaults, delegates to
  `dbmigrate.RunMigrations` with postgres schema token replacements.

## Relationships

- **Depends on**: `db/migrate` (generic migration orchestration),
  `db/sqlc` (BackendType enum for driver selection).
- **Depended on by**: `db/actordelivery` (its own `RunMigrations` wraps this
  package's), `db` (`db/sqlite.go` and `db/postgres.go` call this package's
  `RunMigrations` directly alongside the main-schema migration run).

## Invariants

- SQL files are embedded at compile time; adding a new migration requires both
  a new `*.sql` file and bumping `LatestMigrationVersion`.
- Uses the same SQLite→Postgres replacement map as the main schema runner
  (`db/migrate.PostgresSchemaReplacements()`).
- Never edit generated sqlc files in `db/actordelivery/sqlc/` — regenerate via
  `make sqlc`.

## Deep Docs

- [db/migrate/CLAUDE.md](../../migrate/CLAUDE.md) — Generic migration runner.
- [ARCHITECTURE.md](../../../ARCHITECTURE.md) — System-wide package map.
