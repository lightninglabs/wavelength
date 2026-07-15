# internal/cmd/tools/accounting

## Purpose

Standalone admin reporting command for the client-side accounting ledger. It
opens the daemon database — SQLite or Postgres — through the shared `db`
package, reads generated sqlc accounting projections, and emits a text, JSON,
or CSV report with optional BTC fiat conversion.

The command opens the database with `SkipMigrations` (it never alters the
daemon schema) and reads inside a read-only transaction (`db.ReadTxOption`).
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

- **Depends on**: the shared `db` package (`db.NewStoreFromConfig`,
  `db.ReadTxOption`) which selects the sqlite/postgres backend exactly as the
  daemon does; `db/sqlc` generated read queries; `net/http` for optional fiat
  pricing.
- **Depended on by**: operators running e.g.
  `go run ./internal/cmd/tools/accounting --backend sqlite --sqlite.dbfile <waved.db>`.

## Invariants

- Do not embed raw SQL in the command; add accounting projections to
  `db/sqlc/queries/fee_accounting.sql` and regenerate with `make sqlc`.
- Open the database the same way the daemon does (`db.NewStoreFromConfig`) so
  the report works against either backend; never hand-roll a backend-specific
  connection.
- Default to not running migrations; the report inspects the state the daemon
  already owns. Read inside a `db.ReadTxOption` transaction. Note: the read-only
  transaction is enforced by Postgres, but the modernc sqlite driver does not
  reject writes in a read-only transaction — on sqlite the read-only guarantee
  rests on `SkipMigrations` plus the report issuing only SELECTs.
- Do not import Faraday. Add new price providers behind `PriceSource`.
- Keep output stable enough for admin scripts: add fields rather than
  renaming existing JSON/CSV keys.

## Deep Docs

- [docs/accounting_report.md](../../../../docs/accounting_report.md) —
  operator guide: running the command against SQLite or Postgres, output
  formats, fiat conversion, and read-only behavior.
- [docs/fee_ledger.md](../../../../docs/fee_ledger.md) — the ledger this
  command reports on: chart of accounts, per-flow movements, replay safety.
