# tools

## Purpose

Build-tooling package: pins Go tool dependencies (`tools.go`) and hosts
`linters/`, a custom golangci-lint plugin enforcing an 80-column line
limit that is tab- and log-call-aware.

## Key Types

- `linters.LLPlugin` — golangci-lint plugin implementing the `ll`
  linter (`register.LinterPlugin`); reports lines exceeding
  `LLConfig.LineLength` after expanding leading tabs to
  `LLConfig.TabWidth` spaces, skipping `//go:` directives, import
  blocks, and lines matching `LLConfig.LogRegex` (structured log
  calls, which may wrap args across lines).
- `linters.New(settings)` — Plugin constructor golangci-lint calls via
  `.custom-gcl.yml`; fills default line length (80), tab width (8),
  and log regex when unset.

## Relationships

- **Depends on**: `golangci-lint`/`plugin-module-register`,
  `golang.org/x/tools/go/analysis` (analyzer framework).
- **Depended on by**: `make lint` / `make lint-changed` /
  `make install-custom-gcl` (builds `custom-gcl` per
  `.custom-gcl.yml`, which registers this module as a plugin).

## Invariants

- `tools.go` is guarded by `//go:build tools` and never compiled into
  the main binary; it exists only to pin tool versions in `go.mod`.
- The `ll` linter's log-line skip relies on `LogRegex` matching the
  start of a structured log call; changing log helper naming
  conventions requires updating `defaultLogRegex` here too.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map
