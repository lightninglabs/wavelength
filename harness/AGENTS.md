# harness

## Purpose

In-process integration test environment for the Ark operator and client daemons.
Manages a full Bitcoin regtest stack (bitcoind, LND nodes), an in-process arkd
server, and client daemon processes with controlled mailbox connections.

## Key Types

- `ArkHarness` — Main test harness: spins up bitcoind, LND, arkd, and client
  daemons. Provides chain control (mine blocks, fund wallets) and lifecycle
  management (start/stop/restart).
- `ArkHarnessOptions` — Configuration for harness (client options, seal
  predicates, round settings).
- `ClientDaemonHarness` — Per-client daemon wrapper with gRPC connections and
  `TriggerRoundRegistration()` helper for controlled round participation.
- `ControlledMailboxClient` — Test double that intercepts mailbox message
  delivery. Supports pausing/resuming specific message types to test ordering
  and restart scenarios.
- `IndexerTestClient` — Lightweight client that connects to the indexer service
  for querying VTXOs, rounds, and OOR events in tests.

## Relationships

- **Depends on**: `clientconn` (bridge wiring), `lndbackend` (chain source),
  `db` (server persistence), `rounds` (round actor wiring), `oor` (OOR actor
  wiring), `indexer` (indexer wiring), `mailbox` (controlled mailbox edges).
- **Depended on by**: `itest` (integration tests), `systest` (system tests).

## Invariants

- Each `ClientDaemonHarness` gets a unique name and data directory.
- `ControlledMailboxClient` must be used for tests that require deterministic
  message ordering or pause/resume of specific RPC types.
- The harness manages the full lifecycle; tests must not start/stop bitcoind
  or LND directly.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
