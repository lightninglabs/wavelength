# systest

## Purpose

System-level end-to-end tests, gated by the `systest` build tag, that
exercise real components against Docker-backed Bitcoin/LND infrastructure.
Some tests drive only the boarding-wallet actor via `SysTestHarness`; others
(`send_vtxo_test.go`, `leave_strand_test.go`, `refresh_strand_test.go`,
`send_onchain_strand_test.go`, `vhtlc_recovery_test.go`) stand up a full
in-process `darepod` daemon plus round/serverconn/mailbox pieces for round
and OOR-send scenarios.

## Key Types

- `SysTestHarness` — Per-test wrapper around `harness.Harness` (Docker
  bitcoind + lnd) plus a per-test `actor.ActorSystem`, in-memory SQLite
  `db.BoardingWalletStore`, and subsystem loggers. `NewSysTestHarness`
  isolates every test's Docker infra, actor system, and database.
- `BoardingWalletFixture` — Higher-level fixture built on
  `SysTestHarness`: wires a chain source actor, `wallet.BoardingBackend`, and
  a running `wallet.Ark` actor, and exposes helpers
  (`CreateBoardingAddress`, `FundAddress`, `WaitForBalance`,
  `RegisterNotifier`, `AssertAddressStored`, `AssertIntentStored`) so
  boarding tests skip setup boilerplate.
- `ParallelN(t)` / `TestMain` — Caps concurrent systest execution via a
  semaphore sized by the `-test.parallelism` flag (default 4), since each
  test's Docker harness is resource-heavy.

## Relationships

- **Depends on**: `harness` (Docker bitcoind/lnd test environment), `wallet`
  (boarding wallet actor under test), `chainsource`/`chainbackends`/
  `lndbackend` (chain backend wiring), `darepod` (full in-process daemon for
  round/send-VTXO tests), `db` (test-scoped SQLite stores).
- **Depended on by**: nothing (test-only, `systest`-tagged).

## Invariants

- All files require the `systest` build tag; nothing here compiles into
  default builds.
- Background goroutines and actor systems are per-test, not shared: each
  `SysTestHarness`/`BoardingWalletFixture` cleans itself up via `t.Cleanup`,
  so tests must not share a harness across `t.Parallel()` subtests.
- Tests that need to run concurrently must call `ParallelN(t)` (not raw
  `t.Parallel()`) so the Docker-resource semaphore is respected.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
