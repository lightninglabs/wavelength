# cmd/darepod

## Purpose

Daemon entry point. Parses flags, initializes configuration, and starts the
`darepod.Server`. Exposes `--logdir` and `--configfile` flags for persistent
log directory override and config file selection. Conditionally registers the
optional walletrpc subserver (`walletrpc` + `swapruntime` build tags).

## Key Types

- `configureSwapRuntime(cfg)` — Registers `swapclientserver` as an
  `RPCServiceRegistrar` when the `swapruntime` build tag is present.
- `configureWalletRPC(cfg)` — Registers `swapwallet` as an
  `RPCServiceRegistrar` and sets `SwapConfig.SuppressResume=true` when both
  `walletrpc` and `swapruntime` build tags are present. The stub version is a
  no-op.
- `readConfigFile(v, cmd)` — Loads daemon config from the file indicated by
  `--configfile` (defaults to `<datadir>/darepod.conf`). Missing default
  config files are silently ignored; explicitly specified files must exist.
- `bindOORLimitFlags(v, f)` — Binds hyphenated CLI flag names to the
  mapstructure keys used internally (e.g. `oor.limits.max-mailbox-script-bytes`
  → `oor.limits.maxmailboxscriptbytes`).

## Relationships

- **Depends on**: `darepod` (Server orchestrator), `swapclientserver`
  (`swapruntime` tag only), `swapwallet` (`walletrpc` + `swapruntime` tags).
- **Depended on by**: nothing (binary entry point).

## Deep Docs

- [docs/daemon_cli_guide.md](../../docs/daemon_cli_guide.md) — CLI reference.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
