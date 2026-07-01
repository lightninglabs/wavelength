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
- `lndStartupTimeout` = 90 seconds — extended timeout for LND bootstrap
  steps (TLS/gRPC readiness, `SERVER_ACTIVE` wallet state, chain sync)
  used instead of `defaultTimeout` because these can take meaningfully
  longer than generic harness operations under serialized systest load.
- Coinbase maturity: 100 blocks + 6-block buffer.

## LND TLS Readiness

- `lndTLSReady(tlsPath string) bool` (backed by `loadClientTLSCredentials`)
  gates LND readiness polling instead of a plain `os.Stat` check on the
  cert path. `os.Stat` only proves the file exists, which can observe a
  partially-written cert during container startup; `lndTLSReady` requires
  the file to parse into gRPC `TransportCredentials` before polling moves
  on to the gRPC state check. `TapdHarness.setupLNDPaths` reuses the same
  helper for its paired LND instance.
