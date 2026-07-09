# harness

## Purpose

Docker-based Bitcoin/LND integration test environment. Manages bitcoind and LND
containers with network isolation for end-to-end testing.

## Key Types

- `Harness` — Top-level test harness owning bitcoind, lnd, and arkd lifecycle.
  The primary `lnd` node always runs the bitcoind chain backend; additional
  LND nodes started via `StartAdditionalLNDWithBackend` may instead run
  neutrino.
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
- `StartAdditionalLND(name string) *LndInstance` — Starts an extra LND node
  backed by bitcoind (the default chain backend).
- `StartAdditionalLNDWithBackend(name, chainBackend string) *LndInstance` —
  Starts an extra LND node with an explicit chain backend
  (`LNDChainBackendBitcoind` or `LNDChainBackendNeutrino`). Neutrino syncs
  and broadcasts over the regtest bitcoind's P2P interface (compact block
  filters) instead of RPC/ZMQ, exercising lnd's native SPV / 1p1c broadcast
  path — used to test the lnd-backed `chainbackends`/`lndsubmitter`
  best-effort package broadcast when a light client cannot return a real
  package-accept verdict.

## Relationships

- **Depends on**: `chain` (bitcoind RPC client helpers), `lndclient` (drives
  and queries the LND containers it starts).
- **Depended on by**: `systest` (system-level tests, which separately wire
  `chainbackends`/`lndbackend` types around the LND instances this harness
  starts).

## Key Constants

- `numInitialBlocks` = 106, `defaultTimeout` = 30s, `pollInterval` = 200ms.
- `BitcoindRPCUser` / `BitcoindRPCPass` — RPC credentials shared across
  tests.
- `electrsReadyTimeout` = 2 minutes — separate extended timeout for the
  electrs container HTTP readiness check.
- `LNDChainBackendBitcoind` / `LNDChainBackendNeutrino` — Chain-backend
  selectors for `StartAdditionalLNDWithBackend`; the primary `lnd` node is
  always started with `LNDChainBackendBitcoind`.
- Coinbase maturity: 100 blocks + 6-block buffer.
