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

## Deep Docs

- [README.md](README.md) — Full topology diagram, subcommand reference, and
  usage walkthrough.
- [harness/CLAUDE.md](../harness/CLAUDE.md) — Server-side `ArkHarness` that
  `start` delegates to.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
