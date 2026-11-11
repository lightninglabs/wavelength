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

## Provider job ownership

Each provider job has a claim token and lease. An installation also has a
delivery lease shared by all its routes. That second lease prevents two replicas
from claiming different routes on one device before the first result records
its quiet period. Provider I/O stays outside both SQL transactions.

Completion fences both claims. It marks only the sent generation done; newer
work stays pending. Accepted sends update the installation's scope-specific
quiet period. A stale result cannot release a newer device lease or disable a
rotated token. Invalid destinations retain a bounded obligation for signed token
recovery instead of blocking an aggregated source's cumulative ACK.

Payment correlators retain bounded replay evidence across presentation changes.
Ordinary wake ingestion uses its durable checkpoint and grant generation rather
than allocating a long-lived dedup row for every wake. Indexed cleanup removes
expired jobs, replay records and subscriptions in bounded transactions.

The job race tests use separate database pools for competing workers and lease
takeover. They also cover N+1 during N's send, quiet periods across routes, token
rotation/recovery, presentation cutover, transaction rollback, replay quota
isolation and independent expiry horizons. HTTP/source ingress wiring follows
in the next slice.
