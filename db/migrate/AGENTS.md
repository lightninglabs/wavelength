# db/migrate

## Purpose

Generic database migration orchestration for SQLite and PostgreSQL backends.
Wraps `golang-migrate` with downgrade protection, per-step callbacks, an
on-the-fly SQLite→Postgres token replacer, and structured logging. Used by
the main schema (`db/`), the actor-delivery sub-schema
(`db/actordelivery/migrations/`), and `sdk/swaps`. The migration driver is
build-tagged: native builds (`driver_native.go`) use golang-migrate's
sqlite/postgres drivers, js/wasm builds (`driver_wasm.go`,
`sqlite_wasm_driver.go`) use a hand-rolled `wasmSQLiteDriver` to avoid
pulling the modernc sqlite driver into the browser bundle.

## Key Types

- `Target` — Function signature for migration strategies; `TargetLatest` is
  the only predefined strategy, calling `mig.Up()` (apply all pending
  migrations).
- `Config` — Migration control: `MigrationsTable`, `DatabaseName`,
  `LatestVersion` (downgrade guard), `PostStepCallbacks
  map[uint]golangmigrate.PostStepCallback` (Go callback run after the
  matching SQL step applies), `PostgresReplacements map[string]string`
  (SQLite→Postgres token map), optional `Log btclog.Logger`.
- `RunMigrations(db, backend, sourceFS, sourcePath, target, cfg)` — Top-level
  entry point. Builds the driver, wraps the `fs.FS` with `replacerFS` if
  Postgres replacements are configured, creates the golang-migrate instance,
  verifies version state, and applies the `Target`.
- `PostgresSchemaReplacements()` — Returns a copy of the canonical SQLite→Postgres
  replacement map (`BLOB→BYTEA`, `INTEGER PRIMARY KEY AUTOINCREMENT→BIGSERIAL
  PRIMARY KEY`, `INTEGER PRIMARY KEY→BIGSERIAL PRIMARY KEY`,
  `TIMESTAMP→TIMESTAMP WITHOUT TIME ZONE`).
- `ErrMigrationDowngrade` — Sentinel returned when the DB version exceeds
  `LatestVersion`.
- `replacerFS` / `replacerFile` — `fs.FS` wrappers that apply the replacement
  map to file contents on the fly, enabling cross-database schema portability
  without duplicate SQL files.
- `migrationLogger` — Adapts `btclog.Logger` to the golang-migrate logger
  interface with level-aware routing.

## Relationships

- **Depends on**: `db/sqlc` (BackendType enum for driver selection),
  `github.com/golang-migrate/migrate/v4`.
- **Depended on by**: `db` (main schema runner), `db/actordelivery/migrations`
  (actor-delivery schema runner), `sdk/swaps` (store migrations).

## Invariants

- `verifyVersionState` refuses to apply migrations if the DB is dirty (manual
  intervention required) or if the current version exceeds `LatestVersion`
  (downgrade protection).
- `replacerFS` applies replacements on every file read, so SQL files remain
  the SQLite canonical form; Postgres-specific syntax is injected at runtime.
  Replacement key order is computed once per `replacerFS`, with longer keys
  applied first so `INTEGER PRIMARY KEY AUTOINCREMENT` is not partially
  rewritten by the generic primary-key rule.
- `PostStepCallbacks` are invoked after the golang-migrate step number they
  are keyed on; use for data-migration side effects that must run between
  schema steps.

## Deep Docs

- [db/CLAUDE.md](../CLAUDE.md) — Main schema and store overview.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
