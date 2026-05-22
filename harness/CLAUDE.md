# harness

## Purpose

In-process integration test environment for the Ark operator and client
daemons. Manages a full Bitcoin regtest stack (bitcoind, LND nodes), an
in-process arkd server, and client daemon processes with controlled mailbox
connections.

## Key Concepts

Use `go doc harness.<Symbol>` for signatures.

- **`ArkHarness`** — Top-level harness. Chain control (mine, fund), lifecycle
  (start/stop/restart), and direct server-state inspection (e.g.,
  `GetBatchTreeState(roundID, outputIdx)`, `GetServerVTXOStatus(outpoint)`
  bypass RPC). `RestartArkd` reuses the data directory and re-applies any
  `OperatorConfigMutator`; `SkipArkd=true` makes both Restart variants hard
  errors.
- **Funding** — Backend-agnostic via `FundClientWallet` (`NewWalletAddress` +
  `ListWalletUnspent` poll). `FundClientLND` is LND-specific (CPFP fee
  inputs for unroll tests). `FundClientWalletN(amountPerUTXO, count)`
  drives cross-round multi-input unrolls with `count >= num_ancestry_paths`
  so each tree has a distinct UTXO; all faucet outputs confirm in a single
  `Generate(6)` (don't mine between faucet calls). `FundOperatorLNDTaproot`
  produces P2TR outputs for boarding/CPFP.
- **Crash + restart** — `CrashClientDaemon(name)` cancels the daemon root
  context (no graceful shutdown) and starts a replacement against the same
  data directory and wallet; removes the entry from `clientDaemons` before
  cancel so concurrent crash calls don't see a half-crashed entry. The
  replacement reuses the original RPC port slot. `RestartArkdDuring(hook)`
  stops arkd, runs `hook()`, then restarts — used for fraud-response
  restart tests.
- **`ArkHarnessOptions`** — Options bag (client opts, seal predicates, round
  settings, `OperatorConfigMutator`, `OperatorDebugLevel`/`ClientDebugLevel`
  overriding the trace defaults, `RPCTransport`).
- **Indexer test client** — `IndexerTestClient` uses compound mailbox ID
  (`operator:client`) + Schnorr auth. `StartIndexerTestClient` consumes the
  backend-agnostic `IndexerProofKey` capability so it works against both
  `lnd` and `lwwallet` client wallet backends — **don't** reach into
  `daemon.LND.Client.WalletKit`. Also supports submitting prebuilt mailbox
  query requests so offline-recipient visibility tests can reuse a signed
  proof generated before the daemon shut down.
- **Ledger assertion** — `LedgerSnapshot` + `TakeLedgerSnapshot` (built from
  `ListFeeEvents`) capture per-account signed balances, entry count, max
  ID, and per-event-type counts. `ExpectedDelta` describes the expected
  shift; missing keys assert zero (strong unintended-leg guarantee).
  `AssertLedgerDelta` iterates `AllAccounts()`.
- **Fee schedule helpers** — `DefaultItestFeeSchedule` (canonical itest
  schedule with `StaticFeeRateSatKW = chainfee.FeePerKwFloor` so chain
  estimator noise doesn't bleed into assertions), `ZeroFeeSchedule` (also
  pinned), `WithFeesSchedule`, `WithZeroFeeSchedule`. Default applies the
  non-zero schedule and zeros the legacy `MinOperatorFee` (#270 — seal-time
  quote builder is the only fee authority; `MinOperatorFee` remains on
  `OperatorTerms` purely as advertised value for pre-#270 clients).
- **Admin shortcuts** — `SealRoundNow()` calls `TriggerBatch` admin RPC and
  returns the sealed round ID; `require.NotNil` on `ArkAdminClient`. Hard
  error on `SkipArkd` harnesses.
- **RPC transport** — `RPCTransportGRPC` (`"grpc"`) / `RPCTransportREST`
  (`"rest"`) string constants select client transport;
  `ArkHarnessOptions.RPCTransport` defaults to gRPC.
  `ArkRPCGatewayAddr` is populated after `startArkd` when the grpc-gateway
  listener is ready; REST transport waits on it via the `waitForArkd` loop.

## Relationships

- **Depends on**: `adminrpc` (ledger snapshot), `clientconn` (bridge),
  `lndbackend`, `db`, `rounds`, `oor`, `indexer`, `mailbox` (controlled
  edges), `metrics` (disabled by default in tests).
- **Depended on by**: `itest`, `systest`.

## Invariants

- Each `ClientDaemonHarness` gets a unique name + data directory.
- `ControlledMailboxClient` is required for ordering / pause-resume tests.
- The harness owns the full lifecycle — tests must not start/stop bitcoind
  or LND directly.
- Wait for `DaemonReady()` before issuing test RPCs.
- Metrics server is disabled by default to avoid port conflicts.
- Wallet unlock timeout is raised for slower CI.
- Every client daemon has a `bitcoindrpc` `PackageSubmitter` wired for
  unroll CPFP package relay (talks to harness bitcoind via JSON-RPC).
- `FundClientWallet` polls `ListWalletUnspent` until the new confirmed UTXO
  appears, preventing immediate-spend races in unroll tests.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide map.
