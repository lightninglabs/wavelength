# harness

## Purpose

Docker-based Bitcoin/LND integration test environment. Manages bitcoind and LND
containers with network isolation for end-to-end testing.

## Key Types

- `Harness` — Top-level test harness owning bitcoind, lnd, and arkd lifecycle.
- `LndInstance` — Manages an LND container's lifecycle and connection.
- `TapdHarness` — Optional Tapd instance for asset-related tests.
- `Options` — Configuration struct passed to `NewHarness`. Controls image
  tags, artifact directory, log routing, tapd toggle, `GroupName`, and
  `AlwaysKeepArtifacts`.
- `DefaultOptions()` — Returns a populated `Options` with safe defaults.
- `Block` — Mined block header plus txid list; used by mining helpers.
- `BlockHeader` — Verbose bitcoind `getblockheader` RPC representation.
- `ReorgResult` — Describes the branches produced by a reorg: `OldTip`,
  `ForkPoint`, `Disconnected` (old-chain blocks in height order), and
  `Connected` (new replacement blocks in height order).
- `SetPostgresEnabled(enabled bool) bool` — Toggles postgres mode
  programmatically; returns old value for restore-on-cleanup patterns.

## Key Methods (on `*Harness`)

- `Reorg(depth, newBlocks int) ReorgResult` — Invalidates the last `depth`
  blocks via `invalidateblock`, mines `newBlocks` on the fork point (must be
  > `depth`), and waits for the primary LND node to resync.
- `ReorgDepth(depth int) ReorgResult` — Convenience wrapper: `Reorg(depth,
  depth+1)` to produce a strictly longer replacement branch.
- `ReconsiderBlock(hash string)` — Asks bitcoind to reconsider a previously
  invalidated block.

## Relationships

- **Depends on**: `chain` (bitcoind RPC), `lndbackend` (LND integration),
  `chainbackends` (PackageSubmitter interface).
- **Depended on by**: `systest` (system-level tests).

## Key Constants

- `numInitialBlocks` = 106, `defaultTimeout` = 30s, `pollInterval` = 200ms.
- `lndStartupTimeout` = 90s — dedicated longer timeout for LND bootstrap
  and chain-sync waits (`SERVER_ACTIVE`, chain height polling). Separate
  from `defaultTimeout` because serialized systest runs can take longer
  to bring up fresh LND instances.
- `BitcoindRPCUser` / `BitcoindRPCPass` — RPC credentials shared across
  tests.
- `electrsReadyTimeout` = 2 minutes — separate extended timeout for the
  electrs container HTTP readiness check.
- Coinbase maturity: 100 blocks + 6-block buffer.
