# Accounting report and invariant check

The accounting command has two subcommands. `report` (the default, and what
runs when you pass only flags) prints a balance report. `check` evaluates the
ledger's invariants and exits non-zero if any of them is violated. Both are
read-only.

## Report

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

The report can run while the daemon is active. It reads a consistent committed
SQLite WAL snapshot; uncommitted in-flight writes are excluded.

## Container image

The Docker image includes `/usr/local/bin/wavelength-accounting`. Override the
daemon entrypoint to run the reporter against a read-only database mount:

```shell
docker run --rm --entrypoint wavelength-accounting \
    -v "$PWD/client-data:/client:ro" waved:local \
    report --backend sqlite --sqlite.dbfile /client/data/mainnet/waved.db \
    --format json
```

Without `--apply-migrations`, the SQLite connection uses `mode=ro` and deferred
read transactions. It does not inherit the daemon's `BEGIN IMMEDIATE` or
journal-changing pragmas. The report's queries read one committed transaction
snapshot while the daemon continues writing in WAL mode. Keep the database,
WAL, and shared-memory files together in the mount; never use `immutable=1`
against a live database. A read-only filesystem requires the live WAL/SHM
files to be present and readable; failure to open them is an error, not an
empty report.

The report contains cumulative account balances and event totals, not activity
restricted to a daily or monthly window. Archive one JSON result per collection
for a consistent snapshot. Separate CLI calls for different formats can observe
different commits on a live database. Upload the report, not the database or
other daemon files.

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

The command uses the shared `db` package with a dedicated read-only SQLite
opener and the normal Postgres store constructor. Two choices keep default
reporting from changing the source:

- The default SQLite path opens in `mode=ro`, without initializing or migrating
  the database. Postgres uses `SkipMigrations`. Pass
  `--apply-migrations` only when you deliberately want to migrate a database that
  lags the current schema version.
- It reads inside a read-only transaction and issues only `SELECT` queries.

Postgres enforces read-only transactions. Native SQLite enforces read-only
access through the file URI `mode=ro`; it does not rely on the driver honoring
the transaction read-only flag. The explicit `--apply-migrations` maintenance
option instead uses the normal writable store and must not be used by scheduled
report collection.

The command also refuses to open a SQLite path that does not exist. A missing
file usually means a typo, and the shared opener would otherwise create an empty
database and report against it.


## Checking the invariants

```shell
go run ./internal/cmd/tools/accounting check \
    --backend sqlite --sqlite.dbfile ~/.waved/data/waved.db
```

`check` reads the whole ledger in one read-only transaction, evaluates every
invariant against that single snapshot, prints a named result per invariant,
and exits 1 if any of them failed. `--format json` emits the same result as a
JSON document for a CI job or an admin script; `--format text` (the default)
prints one `PASS`/`FAIL` line per check followed by the offending rows.

### Running it against a live daemon

The default SQLite opener is read-only and uses deferred transactions for both
`report` and `check`, so it no longer takes the daemon's immediate write lock.
A long-running read can delay WAL checkpoint progress; keep the timeout bounded
and avoid overlapping checks on a busy daemon. A consistent copy remains useful
for offline investigation but is not required for routine report collection.

Every run prints a **ledger fingerprint**: a SHA-256 over every journal field
in entry-ID order. The fingerprint binds to journal content alone — it names
no database, host, or file — so a check run against a restored copy and one
run against the daemon's own database produce the same hash exactly when their
journals are identical. That is what makes it useful for comparing the two,
and it is also why the run prints `entry_count` and `max_entry_id` next to it:
the hash cannot tell you which of the two you were pointed at.

### What it checks

| Check | What it asserts |
|---|---|
| `trial_balance` | Signed balances across the whole chart of accounts sum to zero, and no entry names an account outside the chart. Near-vacuous by construction — the account columns carry foreign keys into the chart, and double entry closes for any pair of distinct accounts — so a pass here is weak evidence, and the check says so in its own `note`. |
| `wallet_clearing_closure` | `wallet_clearing` nets to exactly zero. It is a transit account; a residue means a sweep booked its input legs without the closing fee and destination legs. A sweep still in flight legitimately holds its inputs there, so re-run once it confirms before treating a residue as a violation. |
| `vtxo_balance_inventory` | Ledger `vtxo_balance` falls within the band the VTXO table allows. The floor is the unspent VTXOs whose ledger removal is synchronous with their status — Live, PendingForfeit, Forfeiting, Spending and Expired. The ceiling adds the ones whose removal is not: a UnilateralExit VTXO flips status at exit *initiation* but keeps its value until the sweep confirms a CSV delay later, and a Failed VTXO has no removing leg at all. Both are named in the check's `note` as reconciling items rather than reported as violations. An expired VTXO the operator has already swept stays counted on both sides, which this check cannot detect. |
| `wallet_balance_audit_reconciliation` | The ledger's `wallet_utxo_created` and `wallet_utxo_spent` legs reconcile with the `wallet_utxo_log` rows that produced them. See the caveat below. |
| `dedup_index_consistency` | No two entries share `(event_type, debit_account, credit_account)` under an equal `round_id`, `session_id`, or `idempotency_key` — one check per partial unique index in migration `000006_accounting.up.sql`. |
| `outpoint_leg_amount_agreement` | The cancelling legs of one outpoint-scoped operation agree in amount. An exit's or a leave's send and proceeds legs both carry the net-of-fee value, because the proceeds leg exists to cancel the send leg on `transfers_out`. The fee leg deliberately differs and is excluded. |
| `audit_ledger_pairing` | Every `wallet_utxo_log` row with a ledger-emitting classification has its ledger leg, and every wallet-UTXO ledger leg has its audit row. Audit-only spent classifications are exempt. |
| `history_non_duplication` | `ListTransactionHistory` returns no two chain-identified rows with the same `(txid, output_index, amount_sat, subtype)`. Only rows carrying a `txid` and a non-negative `output_index` are examined: one output confirms once, so two rows naming it with the same subtype describe one movement. Round-level rows carry no chain identity — an empty `txid` and an `output_index` of `-1` — and the projected columns cannot tell two equal-amount recipients in one round apart from a leg booked twice, so those rows are left to the ledger-level checks, which see the idempotency keys. The history is read in a single 100,000-row page; when it fills, the check says in its `note` that only that prefix was examined. |
| `anomaly_exit_send_followed_flag` | No exit send leg reads `wallet_balance <- vtxo_balance`. |
| `anomaly_oor_self_change_as_transfers_in` | No OOR receive is booked as `transfers_in` under a session that already carries an outgoing `vtxo_sent` leg. |
| `anomaly_deposit_boarding_intent_gap` | Every deposit-classified audit row has its boarding intent, and every boarding intent has its deposit leg. Intents confirmed before the accounting schema existed are excluded: the scan floors at the oldest `wallet_utxo_log` row, since `boarding_intents` predates the ledger by four migrations. |

The last three are **known anomalies**, not hypotheticals: they are exactly the
row shapes the merged accounting fixes stopped producing. A database that
predates those fixes can still hold them, and the fixes do not rewrite history,
so the checker names them so an operator can decide whether to correct them.

### The wallet reconciliation caveat

The daemon database holds no wallet UTXO set. The backing wallet owns that, and
each of the three supported backends exposes it only over its own RPC. What the
database does hold is the `wallet_utxo_log` audit trail, so
`wallet_balance_audit_reconciliation` reconciles the ledger's wallet-UTXO legs
against the audit rows that are supposed to have produced them, and reports the
resulting audit-derived net alongside the `wallet_balance` account. The check
says so in its own `note` field. It is not a chain UTXO-set reconciliation, and
`wallet_balance` legitimately moves on flows the audit log does not record —
boarding, operator fees, and exit or leave proceeds.

### Relationship to the operator-side accounting tool

The two tools share a model and a vocabulary, so an operator who runs both sees
one system rather than two. What carries over:

- **Read-only by default.** Both open the database with migrations skipped and
  read inside a read-only transaction.
- **A content-only journal fingerprint.** Same construction: every journal
  field, length-prefixed, in entry-ID order, with NULL encoded distinctly from
  empty, printed alongside the entry count and maximum entry ID.
- **Double-entry closure** and the vocabulary for it: debits and credits,
  signed versus normal balances, a trial balance that must sum to zero.
- **Named anomalies over inferred repairs.** Neither tool calculates a
  correction from a headline discrepancy; both name the specific rows.

What is client-only, because the operator side has no analogue:

- `vtxo_balance_inventory` — the operator tracks customer claims, not a local
  VTXO inventory it owns.
- `audit_ledger_pairing` and `wallet_balance_audit_reconciliation` — the
  `wallet_utxo_log` audit trail is a client-side table.
- `history_non_duplication` — `ListTransactionHistory` is the client's
  consumer-facing view.
- The three anomaly detectors, which describe client-side producers.

And what the operator side has that the client deliberately does not:

- **A corrections apply mode.** The operator tool can apply a reviewed,
  fingerprint-bound correction plan with an immutable receipt. The client
  checker is report-only. A client ledger is not an operator's book of record,
  the offending rows come from producers this repository controls, and the
  fixes land as migration-time corrections in `db/post_migration_checks.go`,
  where they run once per database with the rest of the schema upgrade. Adding
  a hand-reviewed plan format to every user's daemon would be a second,
  weaker correction path for problems the first one already closes. Route a
  client-side correction through a post-migration check.
