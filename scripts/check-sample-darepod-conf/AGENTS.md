# scripts/check-sample-darepod-conf

## Purpose

CI validation tool that verifies `sample-darepod.conf` documents every
config option exposed by the daemon. It runs `darepod --help` to collect
the canonical flag list, then checks that each non-skipped flag appears at
least once in the sample conf file. Fails with a diff-style report when a
flag is undocumented.

## Relationships

- **Depends on**: `darepod` (for the flag surface via `--help`).
- **Depended on by**: Makefile / CI (invoked by `make doc-check` or equivalent
  sample-conf check target).

## Invariants

- Flags in `skippedFlags` (`configfile`, `help`, `version`) are excluded from
  the check — they are tool flags with no meaningful conf-file representation.
- The tool reads `defaultConfFile` (`sample-darepod.conf`) from the repo root
  and `mainFile` (`cmd/darepod/main.go`) to locate the daemon entry point.

## Deep Docs

- [scripts/CLAUDE.md](../CLAUDE.md) — Parent scripts package overview.
