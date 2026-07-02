# systest

## Purpose

System-level end-to-end integration tests exercising the full daemon with real
Bitcoin/LND backends via the test harness.

## Key Types

- `SysTestHarness` (systest.go) — per-test wrapper around `harness.Harness`
  that also owns a dedicated `actor.ActorSystem`, an in-memory SQLite
  `db.BoardingWalletStore`, and a `btclog.DefaultHandler` for subsystem
  loggers. `NewSysTestHarness` spins up a fresh Docker bitcoind/lnd stack per
  test (`opts.GroupName = t.Name()`); nothing about the harness, actor
  system, or database is shared across tests. `Close` (registered via
  `t.Cleanup`) runs collected cleanup funcs in reverse order, cancels the
  context, then shuts down the actor system.
- `ParallelN(t)` (main_test.go) — use instead of bare `t.Parallel()` for any
  systest that stands up Docker infra. It also blocks on a semaphore sized by
  `-test.parallelism` (default 4), so the suite can't spawn more concurrent
  bitcoind/lnd container sets than the flag allows; a test that calls
  `t.Parallel()` directly bypasses this cap.
- `BoardingWalletFixture` (helpers.go) — wires a `wallet.Ark` boarding wallet
  actor to a `SysTestHarness`'s chain-source actor and LND-backed boarding
  backend. Exposes `CreateBoardingAddress`, `FundAddress`, `WaitForBalance`,
  `RegisterNotifier`/`RegisterNotifierWithBacklog`, and store-assertion
  helpers (`AssertAddressStored`, `AssertIntentStored`).
- `directedSendFixture` (send_vtxo_test.go) — boots a full `darepod.Server`
  (`darepod.NewServer` + `launch`/`restart`) against the systest harness's
  LND and a fake mailbox server, for tests that need the real daemon rather
  than a bare wallet actor (send/leave/refresh-strand and vhtlc recovery
  tests).
- `oorVTXOManagerSystestFixture` (oor_vtxo_manager_test.go) — builds its own
  `db.NewTestDB`-backed `vtxo.VTXOStore` and
  `actor.TxAwareDeliveryStore` (via
  `actordelivery.NewTxAwareDeliveryStoreFromDB`) per fixture instance, and
  each `TestOOR*` test calls it exactly once. Delivery stores are therefore
  never shared between systest cases here — do not hoist one out to a shared
  var across subtests the way `oor.TestNewOORRegistryActorValidatesRequiredDeps`
  does for its constructor-only checks; these systests exercise live actor
  behavior concurrently (e.g. `TestOORConcurrentIncomingMaterialization`) and
  sharing a store/DB would race across tests instead of just wasting Postgres
  connections.

## Relationships

- **Depends on**: `harness` (Docker test environment), `darepod` (daemon
  under test for full end-to-end flows), `baselib/actor`, `wallet`, `vtxo`,
  `oor`, `round`, `ledger`, `db` / `db/actordelivery`, `chainsource`,
  `chainbackends`, `lndbackend`, `serverconn`, `vhtlcrecovery`, `arkrpc`,
  `daemonrpc`, `rpc/oorpb`, `rpc/roundpb`, `mailbox/pb`, `lib/arkscript`,
  `lib/tree`, `lib/tx/oor`, `lib/types`.
- **Depended on by**: nothing (test-only).
