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
- `BitcoindRPCUser` / `BitcoindRPCPass` — RPC credentials shared across
  tests.
- `electrsReadyTimeout` = 2 minutes — separate extended timeout for the
  electrs container HTTP readiness check.
- Coinbase maturity: 100 blocks + 6-block buffer.
- `maxBitcoindStartRetries` = 3 — number of times `startBitcoind` rebuilds
  the bitcoind container when post-start RPC probing fails (guards against
  containers whose port-forward exists but whose bitcoind process never bound
  the RPC socket or crashed during init).
- `bitcoindStartProbeDuration` = 5s — wall-clock window `probeBitcoindHealthy`
  spends polling bitcoind after the first successful `getblockchaininfo`
  response to catch the failure mode where bitcoind answers the first poll
  and then dies before the harness's first user-driven RPC call.

## Helper Functions

- `containerHasExactName(container, name)` — Returns true only when a Docker
  container's name list contains the exact string `"/<name>"`. Needed because
  Docker name filters match substrings; stale containers with overlapping
  prefixes must not be confused for the one under management.
