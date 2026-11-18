# Channel storage

See [Ark channel lifecycle](ark-channel-lifecycle.md) for which facts belong
to this database and which belong to the Ark wallet.

The application opens the channel database and passes it to the modular LND
runtime. LND retains its channel, invoice, payment, and recovery formats. Its
`kvdb/sqlbase` package stores those records through the application SQL driver.

Native endpoints use `channel.sqlite` in their channel data directory, with
the wallet's selected SQLite driver (CGO on Android).
Browser endpoints use `go-wasmsqlite` with persistent OPFS storage, one SQL
connection, and exclusive WAL locking. Node uses the same driver with its
filesystem VFS. Failure to open persistent storage prevents channel startup.

The existing application database continues to hold Ark lifecycle and source
recovery records. These coordinate OOR funding and materialization with LND;
they do not replace its commitment or payment records. SwapDK shares one native channel database and switch across its Ark peers;
lookups and commands remain bound to the authenticated peer. The public lnd
node still has its own database and switch.

## Backups

Back up current SQL state together with the application's Ark recovery state.
Use a consistent SQLite backup or stop the application before copying its
database and WAL files.

## Ownership and testing

`ArkChannelControllerConfig.OpenChannelDB` can supply another compatible
database. Its result belongs to the endpoint. `NativeNodeConfig.OwnsDB`
controls whether the node closes an injected database on failure and stop;
callers sharing database ownership leave it false.

`TestChannelSQLReopen` runs natively and under
WASM. The native funding-flow tests use the same SQL opener as the daemon.
