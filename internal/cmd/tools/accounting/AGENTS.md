# internal/cmd/tools/accounting

## Purpose

Standalone admin reporting command for the client-side accounting ledger. It
opens the daemon SQLite database, reads generated sqlc accounting projections,
and emits a text or JSON report with optional BTC fiat conversion.

## Key Types

- `PriceSource` — small local interface for BTC/fiat price lookups, kept
  independent from Faraday.
- `CoinGeckoPriceSource` — current-price implementation using CoinGecko's
  public simple-price endpoint.
- `report`, `accountBalance`, `eventTotal` — JSON output shapes.

## Relationships

- **Depends on**: `db/sqlc` generated queries through a direct SQLite handle;
  `net/http` for optional fiat pricing.
- **Depended on by**: operators running
  `go run ./internal/cmd/tools/accounting --sqlite <arkd.db>`.

## Invariants

- Do not embed raw SQL in the command; add accounting projections to
  `db/sqlc/queries/fee_accounting.sql` and regenerate with `make sqlc`.
- Do not run migrations from this reporting command; it should inspect the DB
  state the daemon already owns.
- Do not import Faraday. Add new price providers behind `PriceSource`.
- Keep output stable enough for admin scripts: add fields rather than
  renaming existing JSON keys.
