# cmd/darepod

## Purpose

Daemon entry point. Builds the cobra/viper flag surface, loads a
`darepod.Config`, wires optional build-tag-gated subservers onto it, and
hands off to `darepod.Main` to run the daemon.

## Key Functions

- `newRootCmd()` — builds the `darepod` cobra command: registers all
  flags (datadir, network, lnd.*, wallet.*, bitcoind.*, oor.limits.*,
  rpc.* (gRPC TLS/macaroon + gateway), metrics.listen,
  feeestimation.mempoolspace.*, signingworkers, db.sqlite.*, etc.), binds
  them through viper (flag > env > config file > default, with both `.`
  and `-` mapped to `_` for env keys), then runs
  `configureBitcoindSubmitter`, `configureSwapRuntime`, and
  `configureWalletRPC` in `PreRunE` before `run(cfg)`.
- `run(cfg)` — validates the config, wires the daemon log writer, installs
  an OS signal interceptor, and calls `darepod.Main`.
- `configureSwapRuntime(cfg)` / `configureWalletRPC(cfg)` — build-tag-gated
  (see Invariants) hooks that append optional RPC subserver registrars
  (`swapclientserver.Register`, `swapwallet.Register`) onto `cfg`;
  `configureWalletRPC` also appends `swapwallet.ErrorMappingInterceptor`
  to `cfg.UnaryServerInterceptors` so walletdkrpc sentinel errors surface
  as machine-readable gRPC status codes.
- `configureBitcoindSubmitter(v, cfg)` — opt-in direct bitcoind
  `submitpackage` wiring for V3 ephemeral-anchor package relay; a no-op
  when `bitcoind.host` is unset.
- `registerDaemonRPCFlags(f, cfg)` — registers the `rpc.*` flag group
  (listen addr, TLS cert/key, `rpc.notls`, macaroon path,
  `rpc.no-macaroons`, gateway enable/listen/allowed-origins).
- `registerMetricsFlags(f, cfg)` — registers `metrics.listen`; empty
  (the default) disables the Prometheus `/metrics` HTTP server.
- `registerFeeEstimationFlags(f, cfg)` — registers the optional
  `feeestimation.mempoolspace.*` flags that let the lnd wallet backend's
  fee estimator take the lower of the local and mempool.space rates.

## Relationships

- **Depends on**: `darepod` (`Config`, `Main`, the Server orchestrator),
  `swapclientserver` (swap subserver, `swapruntime` tag),
  `swapwallet` (wallet subserver, `walletdkrpc`+`swapruntime` tags),
  `chainbackends/bitcoindrpc` (direct package-relay submitter).
- **Depended on by**: nothing (binary entry point).

## Invariants

- `configureWalletRPC` requires BOTH the `walletdkrpc` and `swapruntime`
  build tags (`walletdkrpc.go` has `//go:build walletdkrpc && swapruntime`);
  a `walletdkrpc`-only build still gets the stub no-op from
  `walletdkrpc_stub.go`, because the wallet subserver composes the daemon's
  swap subsystem and cannot exist without it.
- `configureWalletRPC` runs AFTER `configureSwapRuntime` in `PreRunE`; the
  wallet registrar reads `cfg.Swap.Backend`, which the swap subserver
  registrar publishes, and sets `cfg.Swap.SuppressResume = true` so the
  wallet layer (not the swap subserver) drives the unified startup resume.
- `EagerRoundJoin`'s flag default comes from `darepod.DefaultConfig()`,
  which is itself build-tag aware (true under `walletdkrpc`, false
  otherwise); `--eagerroundjoin` still overrides it either way.
