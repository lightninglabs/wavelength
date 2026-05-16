# scripts/check-sample-darepod-conf

## Purpose

CI verification tool that confirms `sample-darepod.conf` documents every
daemon configuration option at its current default value. Fails if the sample
config is missing an option or has a stale default, preventing the documented
example from drifting out of sync with the actual daemon flags.

## Key Types

- `main` — Collects expected config keys from the daemon's struct tags and
  flag set, parses the sample config file, and reports any missing or
  mismatched entries.

## Relationships

- **Depends on**: `darepod` (Config struct and default values), standard
  `os`/`exec`/`reflect` packages.
- **Depended on by**: nothing at runtime; invoked by CI or `make` as a
  pre-commit check.

## Invariants

- Any new field added to `darepod.Config` with a mapstructure tag must also
  be added to `sample-darepod.conf` with the correct default value, or this
  tool will fail.

## Deep Docs

- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
