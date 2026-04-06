# cmd/arkd

## Purpose

Daemon entry point for the Ark operator server. Parses configuration, sets up
signal handling, and calls `darepo.Main()`.

## Relationships

- **Depends on**: root `darepo` (server orchestration), `build` (version/logging),
  `config` (metrics server listen address flag).
- **Depended on by**: nothing (top-level binary).

## CLI Flags

- `--metricsaddr` — Prometheus metrics HTTP listen address (opt-in; server only
  starts when set).
