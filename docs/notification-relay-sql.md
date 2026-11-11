# Notification relay SQL storage

The publisher relay has its own database. `notifyrelay/relaydb` opens SQLite or
PostgreSQL and applies the same embedded migrations to either engine. It owns
installation assertions, consumed challenges, subscriptions, source subscriber
keys, feed checkpoints and provider jobs. It contains no wallet state or wallet
signing keys.

This first slice adds the schema, typed management queries and transaction
primitive. The daemon still uses its existing store until the following relay
runtime slices replace it. No import or automatic fallback opens an old store.

## Persistence rules

- SQLite uses a publisher-configured private file, WAL and synchronous commits.
  Its pool has one connection. PostgreSQL uses a dedicated publisher database.
- A source has its own persisted subscriber key. Never reuse that key at an
  unrelated producer: the existing mailbox authentication proof is not bound to
  a producer identity.
- Consumed challenges and installation assertion counters commit with their
  authorized state change. Token revisions cannot decrease. A subscription ID
  cannot change its installation or app route.
- `Database.Write` uses a serializable transaction and retries recognized engine
  conflicts. Its closure contains SQL and local computation only. Reset outputs
  on each attempt. Provider sends and remote mailbox ACKs run after commit.
- Source identity and challenge keys use insert-if-absent queries. Restart must
  restore them rather than regenerate authority over enrolled routes.

## Generation and verification

Run `make relay-sqlc` after changing `notifyrelay/relaydb/queries` or migrations.
The pinned generator produces `notifyrelay/relaydb/sqlc`; do not edit that Go
code. Handwritten Go contains no SQL statements.

`go test -race ./notifyrelay/relaydb/...` exercises SQLite. Add
`-tags=test_postgres` to run the same cases on a disposable PostgreSQL container.
The suite covers pool restart, binary round trips, challenge rollback, concurrent
counter updates, stale token/counter rejection, subscription ownership and source
key retention.
