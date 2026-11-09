# Channel storage

The application opens the channel database and passes it to the modular LND
runtime. LND retains its channel, invoice, payment, and recovery formats. Its
`kvdb/sqlbase` package stores those records through the application SQL driver.

Native endpoints use `channel.sqlite` in their channel data directory.
Browser endpoints use `go-wasmsqlite` with persistent OPFS storage, one SQL
connection, and exclusive WAL locking. Node uses the same driver with its
filesystem VFS. Failure to open persistent storage prevents channel startup.

The existing application database continues to hold Ark lifecycle and source
recovery records. These coordinate OOR funding and materialization with LND;
they do not replace its commitment or payment records. Each logical endpoint
still has isolated storage. Physical database consolidation and a shared
multi-peer runtime are separate changes.

## Existing channel databases

On native startup, an existing `channel.db` is opened read-only. Its bucket
tree, values, and sequences are copied to SQL and verified before one SQL
transaction commits both the copy and its completion marker. An interrupted
or failed transaction can be retried. An unmarked, nonempty destination is
rejected rather than merged with the old database.

The original Bolt file remains an offline backup. After migration, SQL is
authoritative and subsequent opens never replay the old copy. Do not start
an older binary against that backup: it can contain revoked commitments.
Back up current SQL state together with the application's Ark recovery state.
Use a consistent SQLite backup or stop the application before copying its
database and WAL files.

## Ownership and testing

`ArkChannelControllerConfig.OpenChannelDB` can supply another compatible
database. Its result belongs to the endpoint. `NativeNodeConfig.OwnsDB`
controls whether the node closes an injected database on failure and stop;
callers sharing database ownership leave it false.

`TestChannelBoltMigration` verifies rollback, exact copying, and preservation
of newer SQL state on reopen. `TestChannelSQLReopen` runs natively and under
WASM. The native funding-flow tests use the same SQL opener as the daemon.
