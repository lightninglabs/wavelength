# build

## Purpose

Cross-cutting build metadata and logging infrastructure: deployment-mode
constants (Development vs Production), build-tag-controlled log type and log
level, context-based logger propagation, subsystem logger factory, version
string, and log file compressor utilities.

## Key Types

- `DeploymentType` — Enum (`Development`, `Production`). `Deployment` constant
  is set at compile time via the `dev` build tag.
- `LogType` — Enum (`LogTypeNone`, `LogTypeStdOut`, `LogTypeDefault`).
  `LoggingType` constant is set via `stdlog`/`nolog` build tags.
- `LogLevel` — Build-tag-controlled string (`"debug"`, `"info"`, `"warn"`,
  etc.). Consumed by `NewSubLogger` to set the active log level.
- `ContextWithLogger` / `LoggerFromContext` / `MustLoggerFromContext` — Store
  and retrieve a `btclog.Logger` via `context.Context`. `LoggerFromContext`
  returns `btclog.Disabled` (safe no-op) when absent; `MustLoggerFromContext`
  panics when absent.
- `NewSubLogger` — Creates a subsystem logger appropriate for the active
  `Deployment` and `LoggingType`. Returns `btclog.Disabled` for any
  unconfigured combination so callers never receive a nil logger.
- `Version()` — Semver string assembled from `AppMajor`/`AppMinor`/`AppPatch`
  and `AppPreRelease`.
- `Commit` / `CommitHash` / `GoVersion` / `RawTags` — Build-time and
  runtime metadata populated via `-ldflags` and `debug.ReadBuildInfo()`.
- `SupportedLogCompressor` — Checks whether a compression algorithm identifier
  (`Gzip`, `Zstd`) is supported for rotated log files.

## Relationships

- **Depends on**: (no internal repo imports; only `btclog/v2` and standard
  library).
- **Depended on by**: `walletcore`, `waved`, `round`, `ledger`, `oor`,
  `vtxo`, `wallet`, `serverconn`, and most other packages that create
  subsystem loggers or propagate loggers via context.

## Invariants

- `LoggerFromContext` never returns nil; missing logger falls back to
  `btclog.Disabled`. Only use `MustLoggerFromContext` when a logger is
  contractually guaranteed to be in the context.
- `Deployment` and `LoggingType` are compile-time constants resolved by build
  tags, not runtime config. Tests use the `dev` + `stdlog` tags so loggers
  write to stdout without a shared backend.
- Version constants (`AppMajor`, `AppMinor`, `AppPatch`, `AppPreRelease`) must
  be updated manually before release; `Commit`/`CommitHash` are injected
  automatically by the build system.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
