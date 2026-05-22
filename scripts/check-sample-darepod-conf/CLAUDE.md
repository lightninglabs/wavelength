# check-sample-darepod-conf

## Purpose

CI utility that verifies `sample-darepod.conf` documents every daemon config
option with its current default value. Prevents the sample config from drifting
out of sync with `darepod.Config` struct fields and CLI flag registrations.

## Key Types

- `main` — entry point; drives config-key collection, sample parsing, and diff
  reporting.
- `daemonFlag` — name/default pair parsed from `darepod --help` output.

## Relationships

- **Depends on**: `darepod` (reflection over `DefaultConfig()` for expected
  keys), `cmd/darepod` (invoked via `go run` to extract CLI flag defaults)
- **Depended on by**: CI (via `make` or `go run ./scripts/check-sample-darepod-conf`)
- **Sends**: none
- **Receives**: none

## Invariants

- All entries in `sample-darepod.conf` must be commented (`# key=value`);
  any live (uncommented) config line causes a hard failure.
- Every key present in the sample must match the live default; stale defaults
  are reported as mismatches.
- Flags listed in `skippedFlags` (`configfile`, `help`, `version`) are
  intentionally excluded from coverage checks.

## Deep Docs

- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map
