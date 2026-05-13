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
- `--rounds.connectortreeradix` — Connector tree branching factor (default 4).
  Stamped on `ConnectorTreeDescriptor.Radix` at finalization so fraud-response
  path reconstruction is config-rotation-safe.
- `--bitcoind.cookiepath` — Path to bitcoind's cookie auth file. Preferred over
  `--bitcoind.user` / `--bitcoind.pass` for local deployments. When set,
  `bitcoindCreds` reads and parses the file at startup; empty or malformed
  creds cause a hard startup failure.

## Startup Invariants

- When `bitcoind.host` is non-empty, `cmd/arkd` wires `Config.PackageSubmitter`
  with a `bitcoindrpc.New(host, user, pass)` submitter during cobra's
  `PersistentPreRunE` before `run()` validates the config. Failure to resolve
  creds (missing cookie file, empty user/pass) is a startup error.
- `Config.ValidatePackageRelay()` is called in `run()` to confirm the submitter
  is wired when `bitcoind.host` is set. This is separate from `Config.Validate()`
  so pure config file tests can validate without constructing an RPC client.
