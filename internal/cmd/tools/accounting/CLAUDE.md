# internal/cmd/tools/accounting

## Purpose

Standalone admin command for the client-side accounting ledger, with two
subcommands: `report` (the default when only flags are passed) and `check`.

`report`
opens the daemon database — SQLite or Postgres — through the shared `db`
package, reads generated sqlc accounting projections, and emits a text, JSON,
or CSV report with optional BTC fiat conversion.

By default SQLite opens through the shared native opener with `ReadOnly: true`
(`mode=ro`) and deferred read transactions. It does not initialize migrations
or inherit the daemon's immediate write lock. Postgres uses `SkipMigrations`
and a read-only transaction (`db.ReadTxOption`).
For the sqlite backend it also refuses to create a fresh file, erroring on a
missing path instead of reporting against an empty database it just made.

## Key Types

- `config` — parsed CLI flags (backend selector, per-backend connection
  settings, output format, fiat options).
- `PriceSource` — small local interface for BTC/fiat price lookups, kept
  independent from Faraday.
- `CoinGeckoPriceSource` — current-price implementation using CoinGecko's
  public simple-price endpoint.
- `report`, `accountBalance`, `eventTotal` — JSON/CSV output shapes.
- `checkReport`, `checkResult`, `checkFinding` — output shapes for the
  invariant checker; check names are stable identifiers that admin scripts
  key on.
- `checkSnapshot` — every row the checks run against, read in one read-only
  transaction so no two checks can disagree about the ledger's state.

## Invariant check

`check` evaluates the ledger invariants documented in
[docs/fee_ledger.md](../../../../docs/fee_ledger.md) against one snapshot,
prints a named `PASS`/`FAIL` result per invariant with the offending rows, and
returns `errCheckFailed` so the process exits non-zero. Every run prints a
content-only journal fingerprint (`journalHash`), which names no database,
host or file, alongside the entry count and maximum entry ID.

## Flags

- `--backend` — `sqlite` (default) or `postgres`.
- `--sqlite.dbfile` — path to the daemon sqlite database (required for the
  sqlite backend).
- `--postgres.host/.port/.user/.password/.dbname/.ssl` — postgres connection
  settings.
- `--apply-migrations` — off by default; when set, runs migrations before
  reporting (the only path that may mutate schema).
- `--format` — `text` (default), `json`, or `csv`.
- `--price-source` — `none` (default) or `coingecko`.
- `--fiat`, `--timeout`.

## Relationships

- **Depends on**: the shared `db` package (`db.OpenSQLiteDatabase` and
  `db.NewStore` for default read-only SQLite; `db.NewStoreFromConfig` for
  Postgres and explicit migration maintenance; `db.ReadTxOption` for snapshot
  reads); `db/sqlc` generated read queries; `net/http` for optional fiat pricing.
- **Depended on by**: operators running e.g.
  `go run ./internal/cmd/tools/accounting --backend sqlite --sqlite.dbfile <waved.db>`.

## Invariants

- Do not embed raw SQL in the command; add accounting projections to
  `db/sqlc/queries/fee_accounting.sql` and regenerate with `make sqlc`.
- Default SQLite reporting uses `db.OpenSQLiteDatabase` and the shared sqlc
  store adapter with `ReadOnly: true`. Keep daemon initialization, immediate
  write locks, and journal-changing pragmas out of this path. Postgres and
  explicit migration maintenance use `db.NewStoreFromConfig`.
- Default to not running migrations; the report inspects the state the daemon
  already owns. Read inside a `db.ReadTxOption` transaction. Note: the read-only
  transaction is enforced by Postgres. Native SQLite enforces read-only file
  access through `mode=ro`, not the driver's transaction read-only flag.
- Do not import Faraday. Add new price providers behind `PriceSource`.
- Keep output stable enough for admin scripts: add fields rather than
  renaming existing JSON/CSV keys. Check names are part of that contract.
- The checker is report-only and stays that way. Corrections to rows this
  repository's own producers wrote belong in `db/post_migration_checks.go`,
  not in a hand-reviewed correction-plan mode here.
- A new invariant is a new named check with its own test: one seeded
  violation and one clean case.

## Deep Docs

- [docs/accounting_report.md](../../../../docs/accounting_report.md) —
  operator guide: running the command against SQLite or Postgres, output
  formats, fiat conversion, and read-only behavior.
- [docs/fee_ledger.md](../../../../docs/fee_ledger.md) — the ledger this
  command reports on: chart of accounts, per-flow movements, replay safety.
