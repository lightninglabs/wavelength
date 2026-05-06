# harness

## Purpose

In-process integration test environment for the Ark operator and client daemons.
Manages a full Bitcoin regtest stack (bitcoind, LND nodes), an in-process arkd
server, and client daemon processes with controlled mailbox connections.

## Key Types

- `ArkHarness` — Main test harness: spins up bitcoind, LND, arkd, and client
  daemons. Provides chain control (mine blocks, fund wallets) and lifecycle
  management (start/stop/restart). Exposes `GetBatchTreeState(ctx, roundID,
  outputIdx)` for inspecting on-chain batch watcher state from integration
  tests without going through the RPC layer. `FundClientLND(daemon, amount)`
  sends coins directly to a client's backing LND wallet (for CPFP fee inputs
  in unroll tests). `FundClientWallet(daemon, amount)` is backend-agnostic:
  funds the client wallet via `NewWalletAddress` + poll on
  `ListWalletUnspent`, working for both LND and lwwallet backends.
  `FundClientWalletN(daemon, amountPerUTXO, count)` sends `count` independent
  UTXOs of `amountPerUTXO` sats each; tests driving cross-round multi-input
  unrolls call this with `count >= num_ancestry_paths` so each tree's CPFP
  attempt has a distinct available UTXO.
  `CrashClientDaemon(name)` simulates an abrupt client crash: it cancels the
  daemon root context without graceful shutdown and launches a replacement
  against the same data directory and wallet resources. This is the closest
  in-process crash analogue available without spawning a separate OS process.
- `ArkHarnessOptions` — Configuration for harness (client options, seal
  predicates, round settings, `OperatorConfigMutator` for per-test server
  config overrides, `OperatorDebugLevel` / `ClientDebugLevel` for
  per-test log verbosity overrides; both default to `"trace"` when empty).
- `ClientDaemonHarness` — Per-client daemon wrapper with gRPC connections and
  `TriggerRoundRegistration()` helper for controlled round participation.
  Exposes `GetStoredVTXO(ctx, outpoint string)` to retrieve a stored VTXO
  record from the client's DB-backed store for assertion in tests.
- `ControlledMailboxClient` — Test double that intercepts mailbox message
  delivery. Supports pausing/resuming specific message types to test ordering
  and restart scenarios.
- `IndexerTestClient` — Lightweight client that connects to the indexer
  service for querying VTXOs, rounds, and OOR events. Uses compound mailbox
  ID (`operator:client`) and Schnorr auth for identity verification.
  `StartIndexerTestClient` uses the client daemon's backend-agnostic
  `IndexerProofKey` capability to obtain a proof key and signer, so the
  test client works against both `lnd` and `lwwallet` client wallet
  backends. The harness also supports submitting prebuilt mailbox query
  requests so offline-recipient visibility tests can reuse a signed proof
  generated before the client daemon shuts down.
- `LedgerSnapshot` / `TakeLedgerSnapshot` — Point-in-time view of the
  operator's double-entry ledger computed client-side from `ListFeeEvents`
  (admin RPC). Captures per-account signed balances, total entry count,
  max entry ID, and per-event-type entry counts. Used in conjunction with
  `AssertLedgerDelta` to make fee-aware integration tests strongly assert
  exact accounting entries without adding a new server-side API surface.
- `ExpectedDelta` — Describes the expected balance shift, new entry count,
  and per-event-type count increase between two `LedgerSnapshot` values.
  Missing keys in the maps assert a zero delta (strong guarantee against
  unintended ledger legs).
- `AssertLedgerDelta` — Compares two snapshots against an `ExpectedDelta`,
  iterating all accounts from `AllAccounts()`. Any account not in
  `ExpectedDelta.Balances` is asserted to have a zero delta.
- `DefaultItestFeeSchedule` / `WithFeesSchedule` / `WithZeroFeeSchedule` /
  `ZeroFeeSchedule` — Helpers in `fees.go` for managing the operator fee
  schedule in integration tests. `DefaultItestFeeSchedule` returns the
  canonical non-zero itest schedule (lower magnitudes than production) and
  pins `StaticFeeRateSatKW` to `chainfee.FeePerKwFloor` so the chain-backed
  WalletKit estimator does not bleed regtest mempool noise into fee assertions.
  `ZeroFeeSchedule` also pins `StaticFeeRateSatKW` for determinism even on
  the fees-disabled path. The harness applies the non-zero schedule by default;
  tests opt out via `WithZeroFeeSchedule` or customize it via `WithFeesSchedule`.

## Relationships

- **Depends on**: `adminrpc` (ledger snapshot via `OperatorAdminClient`),
  `clientconn` (bridge wiring), `lndbackend` (chain source),
  `db` (server persistence), `rounds` (round actor wiring), `oor` (OOR actor
  wiring), `indexer` (indexer wiring), `mailbox` (controlled mailbox edges),
  `metrics` (disabled by default in tests).
- **Depended on by**: `itest` (integration tests), `systest` (system tests).

## Invariants

- Each `ClientDaemonHarness` gets a unique name and data directory.
- `ControlledMailboxClient` must be used for tests that require deterministic
  message ordering or pause/resume of specific RPC types.
- The harness manages the full lifecycle; tests must not start/stop bitcoind
  or LND directly.
- Harness waits for `DaemonReady()` before issuing test RPCs to avoid races.
- Metrics server is disabled by default in test harnesses to avoid port
  conflicts.
- Wallet unlock timeout is raised in test harnesses to accommodate slower CI
  environments.
- `StartIndexerTestClient` must not reach into backend-specific internals
  (no direct `daemon.LND.Client.WalletKit` access). Use the backend-agnostic
  `IndexerProofKey` capability so the indexer test path stays stable under
  non-LND client wallet backends.
- The harness installs `DefaultItestFeeSchedule` and zeros the legacy
  flat `MinOperatorFee` field by default. Under #270 the seal-time
  quote builder is the only fee authority; the legacy `MinOperatorFee`
  is retained on `OperatorTerms` purely as an advertised value for
  pre-#270 clients. Tests that want a zero schedule (no dynamic fee)
  opt out via `WithZeroFeeSchedule` or an `OperatorConfigMutator`.
- Every client daemon launched by the harness has a `bitcoindrpc` `PackageSubmitter`
  wired for unroll CPFP package relay; this talks directly to the harness
  bitcoind via JSON-RPC.
- `FundClientWallet` polls `ListWalletUnspent` until the new confirmed UTXO
  appears before returning, preventing races in unroll tests that spend from
  the funded wallet immediately after this call.
- `RestartArkd` stops and restarts the in-process arkd server against the
  same data directory, re-applying the `OperatorConfigMutator` if one was
  supplied. Calling `RestartArkd` on a harness constructed with
  `SkipArkd=true` is a hard error.
- `FundClientWalletN` confirms all faucet outputs in a single `Generate(6)`
  call since all transactions land in the same regtest mempool; callers must
  not call `Generate` between individual faucet calls in the loop.
- `OperatorDebugLevel` and `ClientDebugLevel` override the respective
  `DebugLevel` config fields; when either is empty the harness falls back
  to the historical default (`"trace"` for the operator and `"trace,BTCW=debug"`
  for client daemons). Tests that need quieter output may set these to `"info"`
  to reduce log volume without affecting functionality.
- `CrashClientDaemon` removes the old daemon entry from `clientDaemons` before
  cancelling it so concurrent calls for different clients do not observe a
  half-crashed entry. The replacement daemon reuses the original RPC address
  slot after the old listener releases its port.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
