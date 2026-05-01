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
  amounts, and board fanout (number of VTXOs per client at bootstrap).
- `stressSummary` — JSON artifact written to `summary.json` at stress-run
  completion: seed, timing, harness/workload/invariants/recovery results,
  payment counts, failure classes, and recovery failures.
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

## Deep Docs

- [README.md](README.md) — Full topology diagram, subcommand reference, and
  usage walkthrough.
- [harness/CLAUDE.md](../harness/CLAUDE.md) — Server-side `ArkHarness` that
  `start` delegates to.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
