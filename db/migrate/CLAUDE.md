# db/migrate

## Purpose

Generic database migration orchestration for SQLite and PostgreSQL backends.
Wraps `golang-migrate` with downgrade protection, per-step callbacks, an
on-the-fly SQLite→Postgres token replacer, and structured logging. Used by
both the main schema (`db/`) and the actor-delivery sub-schema
(`db/actordelivery/migrations/`).

## Key Types

- `Target` — Function signature for migration strategies, e.g.
  `TargetLatest` or `TargetVersion(n)`.
- `TargetLatest` — Predefined strategy calling `mig.Up()` (apply all pending
  migrations).
- `Config` — Migration control: `MigrationsTable`, `DatabaseName`,
  `LatestVersion` (downgrade guard), `PostStepCallbacks map[uint]func()`
  (called after each step number), `PostgresReplacements map[string]string`
  (SQLite→Postgres token map), optional `Log btclog.Logger`.
- `RunMigrations(db, backend, sourceFS, sourcePath, target, cfg)` — Top-level
  entry point. Builds the driver via `newMigrationDriver`, wraps the `fs.FS`
  with `replacerFS` if Postgres replacements are configured, creates the
  golang-migrate instance, verifies version state, and applies the `Target`.
- `newMigrationDriver(db, backend, migrationsTable)` — build-tagged driver
  factory. `driver_native.go` (`!js || !wasm`) wraps golang-migrate's
  `sqlite`/`postgres` database drivers directly. `driver_wasm.go`
  (`js && wasm`) only supports `sqlc.BackendTypeSqlite` and delegates to
  `newWASMSQLiteMigrationDriver`, avoiding golang-migrate's modernc-backed
  sqlite driver in browser builds.
- `wasmSQLiteDriver` (`sqlite_wasm_driver.go`, `js && wasm` only) —
  hand-rolled `database.Driver` implementation for the browser-backed
  wasmsqlite `database/sql` driver: process-local `Lock`/`Unlock` via
  `atomic.Bool`, transactional `Run`/`SetVersion`, `Version`, and `Drop`
  (drops all non-`sqlite_%` tables then `VACUUM`). `ensureVersionTable`
  creates the `(version, dirty)` bookkeeping table and its unique index
  on first use.
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
  (actor-delivery schema runner), `sdk/swaps` (wasm SQLite swap store
  migrations).

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
