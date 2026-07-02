# cmd/darepod

## Purpose

Daemon entry point (`darepod` binary). Builds the cobra/viper CLI, loads
config from flags/env/config-file, wires optional build-tag-gated
subsystems, then calls `darepod.Main` to construct and run the
`darepod.Server` until an OS shutdown signal.

## Key Types & Functions

- `newRootCmd` — builds the cobra command, registers all flags against
  `darepod.DefaultConfig()`, and binds them to viper (env prefix
  `DAREPOD_`, with `.`/`-` replaced by `_`). Flag registration is split
  across per-subsystem helpers: `registerArkServerFlags`,
  `registerBitcoindFlags`, `registerDaemonRPCFlags`,
  `registerSwapRuntimeFlags`, `registerPprofFlags`,
  `registerMetricsFlags`, `registerFeeEstimationFlags`.
- `PreRunE` load order: `readConfigFile` (properties file) →
  `v.Unmarshal(cfg)` (viper precedence: flag > env > config file >
  default) → `configureBitcoindSubmitter` → `configureSwapRuntime` →
  `configureWalletRPC`. `configureWalletRPC` assumes
  `configureSwapRuntime` already ran — it depends on `cfg.Swap` being
  non-nil and the swap subserver later publishing its backend handle.
- `run` — validates config (`cfg.Validate()`), wires the daemon log
  writer (`configureDaemonLogWriter`), intercepts OS signals via
  `signal.Intercept()` (lnd's `signal` package), then calls
  `darepod.Main(cfg, shutdownInterceptor)`, which constructs
  `darepod.Server` and blocks until shutdown.
- `configureSwapRuntime` / `configureWalletRPC` — build-tag-gated
  registrars vs. no-ops:
  - `swapruntime.go` (build tag `swapruntime`): registers
    `swapclientserver.Register` / `RegisterGateway`.
  - `walletdkrpc.go` (build tags `walletdkrpc && swapruntime`):
    registers `swapwallet.Register` / `RegisterGateway` and
    `swapwallet.ErrorMappingInterceptor`; sets
    `cfg.Swap.SuppressResume = true` because the wallet layer drives a
    unified resume sweep instead of the swap subserver's own.
  - `swapruntime_stub.go` / `walletdkrpc_stub.go` are the
    default-build no-ops so the daemon compiles without these tags.
- `configureBitcoindSubmitter` / `resolveBitcoindAuth` — optional
  direct bitcoind RPC wiring for V3 ephemeral-anchor package relay
  (`submitpackage`). Opt-in via `bitcoind.host`; prefers
  `bitcoind.rpccookie` over `bitcoind.user`/`bitcoind.pass` (mutually
  exclusive) to avoid leaking credentials via `ps` or a persisted
  config file. Warns to stderr (does not refuse) when the host isn't
  loopback and the connection isn't HTTPS.
- `readConfigFile` / `readPropertiesConfig` — minimal `key=value`
  properties-file parser (not YAML/TOML). A missing default config
  file is ignored, but an explicitly-set `--configfile` or
  `DAREPOD_CONFIGFILE` must exist.
- `configureDaemonLogWriter` — makes the standalone binary log to both
  stdout and a persistent `darepod.log` under `cfg.LogDir()`; skipped
  when an embedder already set `cfg.LogWriter` (e.g. `sdk/walletdk`).

## Relationships

- **Depends on**: `darepod` (`Config`, `DefaultConfig`, `Main` — the
  daemon orchestrator), `build` (version string), `chainbackends/bitcoindrpc`
  (optional direct-submit package submitter), `github.com/lightningnetwork/lnd/signal`
  (OS shutdown interception), `spf13/cobra` + `spf13/pflag` + `spf13/viper`
  (CLI parsing/config merge), and, only under build tags,
  `swapclientserver` and `swapwallet`.
- **Depended on by**: nothing (binary entry point).

## Invariants

- Config precedence is flag > env (`DAREPOD_*`) > properties config
  file > `darepod.DefaultConfig()` defaults, enforced by viper.
- `configureSwapRuntime` must run before `configureWalletRPC` in
  `PreRunE` — the wallet registrar reads `cfg.Swap` state that the
  swap registrar establishes.
- This package stays a thin wiring layer: it never talks to the
  network beyond the optional bitcoind cookie/auth read; daemon
  behavior (RPC macaroons, mailbox auth, wallet startup, etc.) lives
  in `darepod`.
