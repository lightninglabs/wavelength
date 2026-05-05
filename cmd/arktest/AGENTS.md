# cmd/arktest

## Purpose

`arktest` is an `itest`-only developer CLI harness for manual end-to-end Ark
testing with the real `arkd`, `arkcli`, `darepod`, and `darepocli` command
surfaces. It starts a local regtest topology (bitcoind + electrs + LNDs +
in-process arkd + one darepod per named client) and exposes subcommands for
mining blocks, funding boarding addresses, printing endpoint info, and
generating shell aliases for the sibling CLI binaries.

## Key Types

- `harnessState` — JSON-persisted topology snapshot written by `start` and
  read by all other subcommands. Holds daemon addresses, per-client state, and
  boarding metadata.
- `arkClientState` — Per-client slice of `harnessState`: RPC address, data
  directory, wallet backend, and optional boarding address/amount.
- `lndState` — LND node endpoints (gRPC addr, TLS cert, macaroon, data dir)
  for both the operator LND and per-client LNDs.
- `startConfig` — Flags parsed by the `start` subcommand (artifact dir, group
  name, client wallet backend, LND image override, funding amounts, client
  names).
- `stressConfig` — Flags for the `stress` subcommand: client count, payment
  budget, round budget, restart budget, concurrency, duration, seed, funding
  amounts, board fanout, and runtime diagnostics settings (trace, CPU/block/
  mutex profiles, liquidity-wait timeout).
- `stressDiagnostics` — Manages Go runtime trace and pprof lifecycle for a
  stress run: starts CPU profiling, sets block/mutex profile rates, starts a
  runtime trace, and stops/flushes all diagnostics at run end.
- `stressDiagnosticPaths` — File paths for the four diagnostic artifacts:
  `trace.out`, `cpu.pprof`, `block.pprof`, `mutex.pprof`.
- `liveVTXOCacheEntry` — Short-lived per-client snapshot of live VTXOs with
  a 250ms TTL; reduces repeated `ListVTXOs` calls to in-process daemons
  during sender selection without letting stale reservations persist.
- `stressSummary` — JSON artifact written to `summary.json` at stress-run
  completion: seed, timing, harness/workload/invariants/recovery results,
  payment counts, skip counts and skip-class distribution, liquidity-wait
  latency percentiles, failure classes, recovery failures, and paths to
  diagnostic artifact files.
- `eventLog` — Sparse, timestamped event logger that mirrors high-level
  arktest events to the terminal and (optionally) a JSON-lines artifact
  (`events.jsonl`). Supports deferred `AttachFile` so the file path can be
  known only after the run directory is allocated by `start`.
- `eventRecord` — Stable JSON-lines shape: `{time, kind, message, fields}`.
- `logTarget` / `newLogsCmd` — Component-log helper used by the `logs`
  subcommand. `logTarget` names a component log path derived from the
  persisted harness state; `newLogsCmd` creates the `logs` subcommand that
  tail-prints or follows the last N lines of any component log.

## Relationships

- **Depends on**: `harness` (server-side `ArkHarness` / `ArkHarnessOptions`),
  `darepo-client/harness` (client-side `DefaultOptions`, `FundClientWallet`,
  `StartClientDaemon`), `darepo-client/daemonrpc` (`NewAddress` RPC for
  boarding address generation), `cobra` (CLI framework).
- **Depended on by**: nothing (top-level developer binary).
- **Build constraint**: `//go:build itest` on all source files — the binary is
  not compiled in normal `make build` targets, only under `itest` build tags.
- **Messages to/from**: none — this is a standalone CLI, not an actor.

## Invariants

- State is persisted to `~/.arktest/current.json` (or `--datadir`) by `start`
  and consumed by all other subcommands; running a subcommand without `start`
  running will fail with a clear "is `arktest start` running?" error.
- `start` uses `testing.Main` deliberately: the test runtime provides lifecycle
  cleanup guarantees (defer chains, `t.Fatal`), not because this is a package
  test.
- `arkd` runs **in-process** inside the binary so no separate arkd process is
  needed; the `harness` package handles wiring.
- Boarding addresses funded via `arktest board` are taproot script-spend
  outputs; they are tracked separately from the LND wallet's key-spend UTXOs
  and must not be picked up by `selectFeeInput` for unroll CPFP children.
- The `--client-wallet=lnd` path (default) spawns a dedicated LND container
  per client so unroll V3 ephemeral-anchor CPFP children have taproot fee
  inputs. Other wallet backends share no per-client LND.
- The `stress` subcommand fans out boarding across multiple VTXOs per client
  (`--board-vtxos`); each VTXO receives `boardAmount / boardVTXOs` sats.
  Fanout shapes that would create VTXOs below `minSatsPerBoardedVTXO` (500 sat)
  are rejected before the harness is started to avoid confusing dust errors.
- `eventLog.AttachFile` buffers events in memory until the final artifact
  directory is known, then flushes the backlog to the JSON-lines file. Callers
  must not assume the file exists before `AttachFile` succeeds.
- The stress workload runs payment attempts concurrently up to `concurrency`;
  the payment loop, round loop, and restart loop all coordinate via the shared
  `stressSummary` counters and a `sync.Mutex`.
- CPU profiling (`cpu.pprof`), block profiling (`block.pprof`, rate 1000), and
  mutex profiling (`mutex.pprof`, fraction 100) are enabled by default on every
  stress run. Pass `--cpu-profile=false` / `--block-profile=false` /
  `--mutex-profile=false` to disable individually.
- Runtime tracing (`--trace`) is opt-in; `--trace-duration` (default 1m)
  auto-stops the trace after the interval so the resulting `trace.out` stays
  small enough for the Go trace browser. Zero duration traces until run end.
- When no sender has enough live spendable balance, payment workers wait up to
  `--payment-liquidity-timeout` (default 10s) before recording a skip. Zero
  timeout records the skip immediately. Waits do not consume the payment
  attempt budget and are tracked separately in `stressSummary.LiquidityWaits`.
- OOR payments in the stress workload do not attach a caller idempotency key,
  so measured fresh-send latency reflects the normal `SendOOR` path, not the
  idempotency-key retry-lookup path.
- The live VTXO cache (250ms TTL per client) prevents redundant `ListVTXOs`
  calls across concurrent payment workers. The cache is invalidated on
  reservation so stale entries do not hide already-reserved outputs.

## Deep Docs

- [README.md](README.md) — Full topology diagram, subcommand reference, and
  usage walkthrough.
- [harness/CLAUDE.md](../harness/CLAUDE.md) — Server-side `ArkHarness` that
  `start` delegates to.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
