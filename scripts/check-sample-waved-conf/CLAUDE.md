# scripts/check-sample-waved-conf

## Purpose

CI validation tool (`main` package) that verifies `sample-waved.conf`
documents every daemon config option with its current default value. It
combines two sources of truth: `waved.DefaultConfig()`, walked via
reflection over `mapstructure` tags, and `waved --help` output (for
CLI-only flags such as `bitcoind.*` that don't live in the config struct).
It cross-checks source-registered flags in `cmd/waved/main.go` against
`--help` to catch parser drift, then diffs the combined expected set
against the sample file's commented `# key=value` entries. Run via `make
sample-conf-check`.

## Key Functions

- `expectedConfigKeys` / `collectConfigKeys` — Recursively reflect over
  `waved.DefaultConfig()` using `mapstructure` tags to build the expected
  key -> default-value map.
- `addDaemonFlagKeys` — Adds CLI-only flags from `waved --help` and
  verifies every flag registered in `cmd/waved/main.go` appears in help
  output.
- `parseSampleConfig` — Parses `sample-waved.conf`; fails if any entry is
  an uncommented live config line.
- `checkSampleConfig` — Diffs expected vs. sample: reports missing keys,
  unknown/stale keys, and mismatched default values.

## Relationships

- **Depends on**: `waved` (`DefaultConfig()` for the config surface; also
  shells out to `go run ./cmd/waved --help` for CLI-only flags).
- **Depended on by**: `make sample-conf-check` Makefile / CI target.

## Invariants

- Flags in `skippedFlags` (`configfile`, `help`, `version`) are excluded —
  tool flags with no meaningful conf-file representation.
- Every entry in `sample-waved.conf` must stay commented (`# key=value`);
  a live (uncommented) line is a hard error.
- `registeredDaemonFlags` assumes daemon flags in `cmd/waved/main.go` are
  registered with literal string arguments; helper/variable-based
  registration needs a parser update alongside that refactor.

## Deep Docs

- [scripts/CLAUDE.md](../CLAUDE.md) — Parent scripts package overview.
