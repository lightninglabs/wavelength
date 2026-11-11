# relaydb

Dedicated SQLite/PostgreSQL persistence for the publisher notification relay.
This package owns schema, typed queries and transaction primitives. It owns no
wallet state, device authentication decisions or provider network requests.

- Regenerate `sqlc/` with `make relay-sqlc`; never edit generated Go.
- Put every query in `queries/`. No SQL statements in Go.
- Both engines run the same migrations. Bind binary columns as byte slices.
- The database is dedicated to this relay; never share a wallet database.
- Preserve assertion counters, consumed challenges, token revisions and claims
  across restart. Provider I/O runs outside SQL transactions.
- Fresh SQL enrollment is intentional. There is no Bolt importer or fallback.
