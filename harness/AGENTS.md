# harness

## Purpose

Docker-based regtest integration-test harness. Spins up bitcoind, electrs
(Esplora HTTP), optional postgres, and one or more LND containers with
per-run network isolation, artifact/log capture, and mining/funding/reorg
helpers for end-to-end tests.

## Key Types

- `Harness` — Top-level test harness owning the bitcoind, electrs, postgres,
  and LND container lifecycle. `NewHarness` builds it; `Start` launches
  containers and `Stop` tears them down. Exposes host ports
  (`BitcoindRPC`, `LNDGRPCPort`, ...), mining (`Generate`,
  `GenerateAndWait`), funding (`Faucet`, `FundOperatorLND`), reorg
  (`Reorg`, `ReorgDepth`, `ReconsiderBlock`), and multi-node helpers
  (`StartAdditionalLND`, `StartAdditionalLNDWithBackend`,
  `SetupChannelBetween`).
- `Options` — `NewHarness` configuration: image tags, `LNDRequireInterceptor`,
  `LNDBuildPath`, `ArtifactsBaseDir`, `GroupName`, log-to-stdout toggles,
  `StartTapd`, `AlwaysKeepArtifacts`. `DefaultOptions()` gives safe defaults.
- `LndInstance` — Handle to one LND container (ports, TLS cert/macaroon
  paths, `*lndclient.LndServices`).
- `TapdHarness` — Paired LND + tapd instance for asset-related tests,
  created via `Harness.NewTapdHarness`.
- `ReorgResult` — Branches produced by a harness reorg: `OldTip`,
  `ForkPoint`, `Disconnected` (old-chain blocks, height order), `Connected`
  (new replacement blocks, height order).
- `LNDChainBackendBitcoind` / `LNDChainBackendNeutrino` — Chain-backend
  selectors for `StartAdditionalLNDWithBackend`, letting a test run one LND
  node over bitcoind RPC+ZMQ and another over neutrino/P2P (BIP157/158) to
  exercise both broadcast paths.

## Relationships

- **Depends on**: `chain` (wraps bitcoind RPC client for
  `SubmitPackage`/package-relay tests via `BitcoindClient()`).
- **Depended on by**: `systest` (system-level tests).

## Invariants

- `NewHarness` only builds the struct; nothing starts until `Start` is
  called. `Start` pre-mines `numInitialBlocks` (106 = 100-block coinbase
  maturity + 6-block buffer) before tests run.
- `Reorg(depth, newBlocks)` requires `newBlocks > depth` so the replacement
  branch is strictly longer than the disconnected one; it blocks until the
  primary LND node resyncs to the new tip.
- `LNDRequireInterceptor` only applies to the primary LND node; additional
  nodes started via `StartAdditionalLND*` never set it.
- Container teardown (`Stop`) is guarded by `sync.Once`; a signal handler
  also calls `Stop` as a safety net against orphaned containers.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
