# Domain shutdown ownership

A host may stop waiting for daemon shutdown at its own deadline. The daemon
still owns its workers and stores until those workers actually return. This
change joins the domain work that previously escaped the shutdown boundary.

| Owner | Work joined before shared-store teardown |
| --- | --- |
| OOR registry | Registry turn, every native session, FSM cleanup and detached terminal/materialization notifications |
| Unroll registry | Registry loop, native children, FSM cleanup, terminal drain probes, asynchronous record writes and abandoned-broadcast cleanup |
| Credit registry | Every native operation, including terminal children already removed from the active map |
| Fraud watcher | Plain actor loop and synchronous watch cleanup |
| RPC server | Detached submitted-transfer cleanup after gRPC handlers have joined |
| Daemon | Native incoming-VTXO, round, server-connection, actor-system and ledger joins before wallets and database close |

Admission follows the actor ownership boundary. A registry stops and joins its
producer loop before waiting for children. Native child counters remain live
through their final access. A detached task increments the same group while
its producer still owns a live turn, and decrements only after its last access.
Removed terminal children therefore remain owned without a second active map.

OOR and unroll use the native Read/Commit path, whose framework does not invoke
a behavior's `OnStop`. Their wrappers cancel the native actor, join its turn,
and only then stop the mutable FSM. Stable-ID watch registration keeps its
existing lifecycle; shutdown does not add a late unregister for a new child. A session stop
cannot race a still-running turn's FSM access. Cleanup tasks are counted before
cancellation can release the native worker.

The daemon's internal join uses its cleanup-owner context without a UI timeout.
The SDK's `CloseContext` still bounds each host's wait and retains the cleanup
owner after a timeout. A slow worker can leave the host in stopping state; it
cannot justify opening the same database under another wallet generation.

This is a shutdown ownership boundary, not a complete background execution API.
It does not certify mailbox catch-up, receipt projection or payment completion.
Optional HTTP diagnostics and future producer lifetimes need their own explicit
host-mode configuration. An empty mailbox alone proves none of these states.

Detached outgoing-transfer RPC cleanup keeps its existing ten-minute ceiling
(`submittedOORCleanupTimeout`). Daemon shutdown joins that cleanup before the
wallet actor stops. A slow or unsettled future can therefore retain shutdown
until that bound; this PR does not promise a hard background completion time.
Detached unroll admission watchers also observe registry shutdown, so an
unclaimed Ask future cannot retain their join indefinitely.
