# scripts/check-sample-darepod-conf

## Purpose

CI verification tool ensuring `sample-darepod.conf` documents every daemon
configuration option with the correct current default value. Detects missing,
stale, or mismatched config entries so the sample file stays in sync with
`darepod.DefaultConfig()` and the daemon's flag registrations.

## Key Types

- `daemonFlag` — Internal struct pairing a flag name with its default value.
- `expectedConfigKeys()` — Extracts config keys and defaults from
  `darepod.DefaultConfig()` via `mapstructure` reflection, then appends
  daemon-only flags via `addDaemonFlagKeys`.
- `collectConfigKeys(prefix, value, expected)` — Recursively traverses struct
  fields with `mapstructure` tags to build the expected key→default map.
- `addDaemonFlagKeys(expected)` — Appends daemon-only flags extracted from
  `go run ./cmd/darepod --help` output and `cmd/darepod/main.go` source
  (explicit flag aliases and registration calls).
- `parseSampleConfig(confFile)` — Parses commented `# key=value` lines from
  the sample config file.
- `checkSampleConfig(expected, sample)` — Reports missing, stale, and
  mismatched entries; returns non-zero on any discrepancy.
- `daemonHelpFlags()` — Parses `darepod --help` output via regex to extract
  flag defaults.
- `explicitFlagAliases(source)` — Extracts Viper flag aliases from
  `cmd/darepod/main.go` source via regex.

## Relationships

- **Depends on**: `darepod` (`DefaultConfig`, `Config` struct), standard
  library (`reflect`, `regexp`, `bufio`, `strconv`).
- **Depended on by**: CI (`make doc-check` or an equivalent check target).
- **Sends/Receives**: none — standalone binary, no actor framework usage.

## Invariants

- Sample config entries MUST remain commented (`# key=value`); live config
  lines are treated as unknown and flagged.
- Skipped flags (`configfile`, `help`, `version`) are never checked.
- Default values from `--help` output are the authoritative source for
  daemon-only flags not reachable via `DefaultConfig()` reflection.
- Every flag registered in `main.go` source must appear in `--help` output;
  a mismatch is a CI failure.

## Deep Docs

- [scripts/CLAUDE.md](../CLAUDE.md) — Scripts directory overview.
- [docs/daemon_cli_guide.md](../../docs/daemon_cli_guide.md) — Daemon config
  reference.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
