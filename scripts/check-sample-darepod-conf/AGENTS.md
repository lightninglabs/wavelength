# check-sample-darepod-conf

## Purpose

CI tool (invoked via `make sample-conf-check`) that verifies
`sample-darepod.conf` documents every daemon config option with the correct
current default value. Fails if any key is missing, stale, or carries a
wrong default, keeping the sample file in sync with `darepod.DefaultConfig`
and the daemon CLI flags.

## Key Types

All symbols are package-private; the binary exposes only `main`.

- `expectedConfigKeys` — Extracts config keys and default values from
  `darepod.DefaultConfig()` by reflecting over `mapstructure` struct tags.
- `addDaemonFlagKeys` — Augments the expected map with CLI-only flags
  (e.g. `bitcoind.*`) by parsing `darepod --help` output and cross-checking
  against flags registered in `cmd/darepod/main.go`.
- `parseSampleConfig` — Reads commented `# key=value` lines from the sample
  file; rejects any live (uncommented) config line.
- `checkSampleConfig` — Reports three failure classes: missing keys, stale
  keys, and default-value mismatches.
- `daemonFlag` — Holds one CLI flag name and its default value as parsed from
  help output.

## Relationships

- **Depends on**: `darepod` (`DefaultConfig()`).
- **Depended on by**: Makefile target `make sample-conf-check`.
- **Sends**: nothing.
- **Receives**: nothing.

## Invariants

- All entries in `sample-darepod.conf` must remain commented; live config
  lines cause `parseSampleConfig` to fail immediately.
- Every flag registered in `cmd/darepod/main.go` must appear in
  `darepod --help`; a mismatch causes `addDaemonFlagKeys` to return an error
  so parser drift is caught early rather than silently producing a stale map.
- The tool does not modify any files; it is read-only and exits non-zero on
  any discrepancy.

## Deep Docs

- [scripts/CLAUDE.md](../CLAUDE.md) — Parent scripts package overview.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
