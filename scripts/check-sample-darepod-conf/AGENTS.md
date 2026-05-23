# scripts/check-sample-darepod-conf

## Purpose

CI check that verifies `sample-darepod.conf` documents every daemon
configuration option with the correct current default value. Run via
`make` to catch config drift when new flags or config struct fields are
added without updating the sample file.

## Key Functions

- `expectedConfigKeys()` — Builds the expected key→default-value map by
  reflecting on `darepod.DefaultConfig()` (mapstructure tags) and
  running `go run ./cmd/darepod --help` to collect daemon CLI-only flags.
- `collectConfigKeys(prefix, value, expected)` — Recursively traverses
  the config struct using reflection, collecting dotted-path keys for
  all mapstructure-tagged fields.
- `formatDefaultValue(value)` — Converts a Go config value to its
  sample-file representation (`time.Duration`, `string`, `bool`,
  integer variants, slices).
- `addDaemonFlagKeys(expected)` — Reads `cmd/darepod/main.go` source and
  the `--help` output to populate keys that live only in the daemon's
  Cobra/Viper layer rather than in `darepod.Config`.
- `parseSampleConfig(confFile)` — Parses the sample file, validates all
  entries are commented out (`# key=value` format), detects duplicates.
- `checkSampleConfig(expected, sample)` — Compares expected vs. sample:
  reports missing keys, unknown/stale keys, and value mismatches.

## Relationships

- **Depends on**: `darepod` (imports `DefaultConfig()` for reflection),
  `cmd/darepod` (runs via `go run` for `--help` flag parsing).
- **Depended on by**: CI via Makefile (`make check-sample-conf` or
  equivalent target).

## Invariants

- The sample file must have ALL config entries commented out. Any live
  (uncommented) line causes the check to fail.
- Key comparison is case-insensitive (mapstructure normalizes to
  lowercase).
- Mismatched defaults are reported as errors, not warnings, because a
  sample file showing a wrong default misleads operators.

## Deep Docs

- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
