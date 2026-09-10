# cmd/waved

## Purpose

Daemon entry point. Builds the cobra/viper flag surface, loads a
`waved.Config`, wires optional build-tag-gated subservers onto it, and
hands off to `waved.Main` to run the daemon.

## Key Functions

- `newRootCmd()` — builds the `waved` cobra command: registers all
  flags (datadir, network, lnd.*, wallet.*, bitcoind.*, oor.limits.*,
  db.sqlite.*, etc.), binds them through viper (flag > env > config
  file > default), then runs `configureBitcoindSubmitter`,
  `configureSwapRuntime`, and `configureWalletRPC` in `PreRunE` before
  `run(cfg)`.
- `run(cfg)` — validates the config, wires the daemon log writer, installs
  an OS signal interceptor, and calls `waved.Main`.
- `configureDaemonLogWriter(cfg, stdout)` /
  `configureDaemonLogWriterWithConfig(...)` — points standalone `waved` at a
  rotating log file (via `jrick/logrotate`) plus stdout, using lnd's defaults:
  a 20 MB threshold and ten gzip backups. Returns the closable writer.
- `serializedLogWriter` — wraps the rotator so the shared sink is serialized.
- `configureSwapRuntime(cfg)` / `configureWalletRPC(cfg)` — build-tag-gated
  (see Invariants) hooks that append optional RPC subserver registrars
  (`swapclientserver.Register`, `swapwallet.Register`) onto `cfg`.
- `configureBitcoindSubmitter(v, cfg)` — opt-in direct bitcoind
  `submitpackage` wiring for V3 ephemeral-anchor package relay; a no-op
  when `bitcoind.host` is unset.
- `registerOperatorFeeFlags(f, cfg)` / `registerMaxPaymentCLTVFlag(f, cfg)` /
  `registerFeeEstimationFlags(...)` — grouped flag registrars called from
  `newRootCmd`. `registerMaxPaymentCLTVFlag` owns `--maxpaymentcltv`, the
  automatic-maintenance target for keeping enough VTXO lifetime available for
  Lightning payments.

## Relationships

- **Depends on**: `waved` (`Config`, `Main`, the Server orchestrator),
  `swapclientserver` (swap subserver, `swapruntime` tag),
  `swapwallet` (wallet subserver, `wavewalletrpc`+`swapruntime` tags),
  `chainbackends/bitcoindrpc` (direct package-relay submitter),
  `jrick/logrotate/rotator` (standalone rotating log file).
- **Depended on by**: nothing (binary entry point).

## Invariants

- `configureWalletRPC` requires BOTH the `wavewalletrpc` and `swapruntime`
  build tags (`wavewalletrpc.go` has `//go:build wavewalletrpc && swapruntime`);
  a `wavewalletrpc`-only build still gets the stub no-op from
  `wavewalletrpc_stub.go`, because the wallet subserver composes the daemon's
  swap subsystem and cannot exist without it.
- `configureWalletRPC` runs AFTER `configureSwapRuntime` in `PreRunE`; the
  wallet registrar reads `cfg.Swap.Backend`, which the swap subserver
  registrar publishes, and sets `cfg.Swap.SuppressResume = true` so the
  wallet layer (not the swap subserver) drives the unified startup resume.
- `lnd.account` is registered here but enforced in `waved`
  (`validateLndAccount`, which refuses to start on a configured account lnd
  does not have). Leaving it empty means lnd's `default` account, so a daemon
  sharing an lnd node with another daemon must set it or the two can drain
  each other's funds.
- `EagerRoundJoin`'s flag default comes from `waved.DefaultConfig()`,
  which is itself build-tag aware (true under `wavewalletrpc`, false
  otherwise); `--eagerroundjoin` still overrides it either way.
- **The rotating log sink is serialized, and only for standalone builds.**
  Subsystem loggers write concurrently while the rotator supports a single
  writer, so `serializedLogWriter` funnels the shared sink through one lock;
  its `Close` waits for the active write before closing the rotator. A caller
  that supplies `cfg.LogWriter` (an embedder) short-circuits the whole path
  and retains full ownership of logging. Stdout mirroring is unaffected.
- `--maxpaymentcltv`'s default is build-tag aware the same way: it reads
  `cfg.MaxPaymentCLTV`, which `waved.DefaultConfig()` seeds from
  `defaultMaxPaymentCLTV()` — 300 under `swapruntime`, zero otherwise. The
  help text therefore differs between builds, and passing `0` explicitly
  disables the payment reserve in a swap-enabled build.
