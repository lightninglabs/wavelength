# Accounting report command

The accounting report command reads the daemon's double-entry fee ledger and
prints a balance report. It opens the database the daemon already owns, sums
each account, totals each ledger event type, and writes the result as text,
JSON, or comma-separated values (CSV). It never changes the database.

The command lives at `internal/cmd/tools/accounting`. For the ledger it reads
— the chart of accounts, the per-flow account movements, and replay safety —
see [fee_ledger.md](fee_ledger.md).

## Running it

Point the command at the database file a SQLite daemon writes:

```shell
go run ./internal/cmd/tools/accounting \
    --backend sqlite --sqlite.dbfile ~/.waved/data/waved.db
```

Against a Postgres daemon, pass the connection settings instead:

```shell
go run ./internal/cmd/tools/accounting \
    --backend postgres \
    --postgres.host localhost --postgres.port 5432 \
    --postgres.user waved --postgres.password "$PGPASSWORD" \
    --postgres.dbname waved
```

Stop the daemon before you run the report, or expect a snapshot that is one
write behind. The command opens the same SQLite file the daemon holds under
write-ahead logging, so it reads a consistent committed state but not
uncommitted in-flight writes.

## Flags

| Flag | Default | Meaning |
|------|---------|---------|
| `--backend` | `sqlite` | Database backend: `sqlite` or `postgres`. |
| `--sqlite.dbfile` | — | Path to the daemon's SQLite database. Required for the SQLite backend. |
| `--postgres.host` | `localhost` | Postgres host. |
| `--postgres.port` | `5432` | Postgres port. |
| `--postgres.user` | `postgres` | Postgres user. |
| `--postgres.password` | — | Postgres password. |
| `--postgres.dbname` | `waved` | Postgres database name. |
| `--postgres.ssl` | `false` | Require TLS for the Postgres connection. |
| `--apply-migrations` | `false` | Run migrations before reporting. Off by default so the report leaves the schema untouched. |
| `--format` | `text` | Output format: `text`, `json`, or `csv`. |
| `--price-source` | `none` | Fiat price source: `none` or `coingecko`. |
| `--fiat` | `usd` | Fiat currency code for the conversion. |
| `--timeout` | `15s` | Deadline for the whole report. |

## Output formats

The text format prints an operator-readable summary: the ledger entry count and
time span, then one line per account, then the event totals.

```
Accounting report (sqlite backend)
Generated: 2026-06-23T18:26:46-07:00
Ledger entries: 142

Accounts
  wallet_balance              4500000 sat  asset
  vtxo_balance                1200000 sat  asset
  fees_paid                      8200 sat  expense
  ...

Events
  boarding_fee_paid                12 entries          6000 sat
  ...
```

The JSON format (`--format json`) emits the same data as one document with
`accounts` and `events` arrays, suited to scripts and dashboards. The CSV format
(`--format csv`) emits one flat table. A leading `category` column marks each row
as an `account` or an `event`, so both kinds share a single stream:

```
category,id,name,account_type,entry_count,amount_sat,fiat
account,wallet_balance,Wallet Balance,asset,,4500000,
account,wallet_clearing,Wallet Sweep Clearing,asset,,0,
event,boarding_fee_paid,,,12,6000,
```

## Fiat conversion

By default the report omits fiat values. Pass `--price-source coingecko` to fetch
the current Bitcoin price from CoinGecko's public endpoint and add a fiat column
to each account balance. Choose the currency with `--fiat` (for example
`--fiat eur`). The command fails rather than report a wrong number when the price
fetch fails, so a network error aborts the report instead of zeroing the fiat
column.

## How the report stays read-only

The command opens the database through the same `db` package the daemon uses, so
one code path serves both backends. Two choices keep the report from changing
anything:

- It opens with `SkipMigrations`, so it never alters the schema. Pass
  `--apply-migrations` only when you deliberately want to migrate a database that
  lags the current schema version.
- It reads inside a read-only transaction and issues only `SELECT` queries.

Postgres enforces the read-only transaction and rejects any write. The
`modernc.org/sqlite` driver does **not** — it accepts writes inside a read-only
transaction. On SQLite, therefore, the read-only guarantee rests on
`SkipMigrations` plus the report issuing only reads, not on the transaction
itself.

The command also refuses to open a SQLite path that does not exist. A missing
file usually means a typo, and the shared opener would otherwise create an empty
database and report against it.
