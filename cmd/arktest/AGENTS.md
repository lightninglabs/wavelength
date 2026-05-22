# cmd/arktest

## Purpose

`itest`-only developer CLI harness for manual end-to-end Ark testing with the
real `arkd`/`arkcli`/`darepod`/`darepocli`. Starts a regtest topology
(bitcoind + electrs + LNDs + in-process arkd + one darepod per named client)
and exposes subcommands for mining, funding, info, stress runs, and shell
aliases.

Build constraint: `//go:build itest` — not compiled by `make build`.

## Subcommands

`start`, `stop`, `mine`, `board` (alias: `new-boarding-address`), `faucet`,
`info`, `stress`, `logs`, `aliases`. State persists to
`~/.arktest/current.json` (or `--datadir`); any subcommand without a running
`start` errors with "is `arktest start` running?".

## Key Concepts

Use `go doc` (under `-tags itest`) for signatures. Highlights:

- **State files** — `harnessState` (full JSON snapshot written by `start`)
  composes `arkClientState` (per-client RPC/data dir/wallet backend/boarding
  metadata) and `lndState` (per-LND endpoints).
- **`startConfig`** — Flags for `start` (artifact dir, group name, client
  wallet backend, LND image override, funding amounts, client names).
- **`stopConfig`** — `timeout` (default 30s graceful) and `force` (SIGKILL
  escalation). Sends SIGINT and polls every `stopPollInterval = 250ms` for
  the state file to disappear.
- **`faucetResponse`** — JSON output of `faucet` (`{address, amount_sat,
  txid, mined_blocks, miner_address, block_hashes}`); sends to any address
  and mines 6 confirmations.
- **`stressConfig`** — Stress flags: clients, payment/round/restart budgets,
  concurrency, duration, seed, board fanout, liquidity-wait timeout,
  unroll budget (`--max-unrolls`, default 5), background-mine cadence
  (`--mine-interval-min`/`--max`, defaults 2s/10s — zero min disables),
  per-unroll wait (`--unroll-timeout`, default 15m), runtime diagnostics.
- **`stressDiagnostics`** — Manages runtime trace + CPU/block/mutex pprof
  lifecycle for a stress run. Artifact paths in `stressDiagnosticPaths`.
- **`stressSummary`** — `summary.json` at run end. Includes payment/skip
  counts + skip-class distribution, liquidity-wait percentiles, failure
  classes, recovery failures, unroll tracking (`UnrollsAttempted/Completed/
  Failed/Skipped`, latency percentiles) and background-mine counters.
- **`eventLog`** — Sparse timestamped logger; mirrors high-level events to
  terminal and optionally `events.jsonl`. `AttachFile` buffers events until
  the artifact directory is allocated, then flushes.
- **`liveVTXOCacheEntry`** — Per-client live-VTXO snapshot with 250 ms TTL,
  invalidated on reservation; avoids stale reservations under concurrent
  sender selection.
- **`logTarget` / `newLogsCmd`** — `logs` subcommand: tail / follow any
  component log derived from persisted harness state.

## Relationships

- **Depends on**: `harness` (`ArkHarness`/`ArkHarnessOptions`),
  `darepo-client/harness` (`DefaultOptions`, `FundClientWallet`,
  `StartClientDaemon`), `darepo-client/daemonrpc` (`NewAddress`),
  `cobra`.
- **Depended on by**: nothing (top-level binary).

## Invariants

- `start` uses `testing.Main` deliberately for lifecycle cleanup guarantees
  (defers, `t.Fatal`), not because this is a test.
- `arkd` runs **in-process** — no separate process needed.
- Boarding addresses are taproot script-spend; tracked separately from LND
  key-spend UTXOs and must not be picked up by `selectFeeInput` for unroll
  CPFP children.
- `--client-wallet=lnd` (default) spawns one LND container per client so
  unroll V3 ephemeral-anchor CPFP children have taproot fee inputs. Other
  backends share no per-client LND.
- `stress --board-vtxos` rejects fanout that would drop any VTXO below
  `minSatsPerBoardedVTXO` (500 sat) before starting the harness.
- CPU profile (`cpu.pprof`), block profile (`block.pprof`, rate 1000),
  mutex profile (`mutex.pprof`, fraction 100) are on by default. Runtime
  trace (`--trace`) is opt-in; `--trace-duration` (default 1m) auto-stops
  to keep `trace.out` small; zero traces until run end.
- Liquidity waits up to `--payment-liquidity-timeout` (default 10s, 0 =
  immediate skip) don't consume the payment budget; tracked separately.
- OOR payments in stress do **not** attach an idempotency key, so latency
  reflects the normal `SendOOR` path, not the retry-lookup path.
- `btc()` (from `aliases`) wraps
  `docker exec "$ARKTEST_BITCOIND_CONTAINER" bitcoin-cli -regtest` so the
  developer can run arbitrary bitcoin-cli against harness bitcoind without
  knowing the container name.

## Deep Docs

- [README.md](README.md) — Full topology, subcommand reference, walkthrough.
- [harness/CLAUDE.md](../../harness/CLAUDE.md) — Server-side `ArkHarness`.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide map.
