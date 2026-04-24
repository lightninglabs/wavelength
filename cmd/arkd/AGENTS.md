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
- `--rpc.notls` — Explicitly disable TLS for the client-facing gRPC server
  (regtest/dev only). When set, `Config.Validate()` clears `RPC.TLS` so the
  "no TLS config provided" error is suppressed. Must be set if no cert/key
  paths and no `--rpc.tls.autocert` are provided.
