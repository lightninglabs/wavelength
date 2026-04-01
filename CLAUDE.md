# Darepo Agent Guide

This file is a **map**, not a manual. Follow links for details.

## Quick Commands

| Command | Purpose |
|---------|---------|
| `make build` | Compile the project |
| `make lint-native` | Run linter natively, no Docker (fastest on macOS) |
| `make lint-changed` | Run linter on changes vs base (must pass before committing) |
| `make lint-changed-local` | Run native local linter on changes vs base |
| `make install-custom-gcl` | Build native `custom-gcl` for this host |
| `make fmt` | Format all Go source files |
| `make unit pkg=<pkg> timeout=5m` | Run unit tests |
| `make unit-debug log="stdlog trace" pkg=<pkg> case=<test>` | Unit tests with debug logs |
| `make itest icase=<test>` | Integration test |
| `make systest` | System-level end-to-end tests |
| `make tidy-module-check` | Verify module files are tidy |
| `make rpc` | Regenerate protobuf stubs |
| `make sqlc` | Regenerate type-safe DB queries |
| `make ast-lint` | Check ast-grep style rules |
| `make submodule-update` | Update client submodule to latest |

## Code Style (Summary)

- **8-space tabs** (see `.editorconfig`), 80-char line limit (best effort).
- Every function/method gets a comment starting with its name.
- Exported identifiers need GoDoc comments wrapped to 80 columns.
- Organize code into logical stanzas with explanatory comments between them.
- Function **calls**: closing `)` on its own line when wrapping.
- Function **definitions**: first param on same line, closing `)` with last param.
- Structured logging: use `InfoS`/`DebugS`/etc. with static messages and
  `slog.Int()`/`btclog.Fmt()` key-value pairs. See [`docs/structured-logging.md`](docs/structured-logging.md).
- `error` log level is **only** for internal bugs, never external triggers.

Full style guide with examples: [`docs/development_guidelines.md`](docs/development_guidelines.md)

## Git Commits

```
pkg: Short summary in present tense (<=69 chars)

Body wrapped at 72 characters. Explain WHY, not just WHAT.
```

- Prefix with package name (`db:`, `rpc:`, `multi:` for multiple).
- Small, atomic commits. Separate bug fixes, refactors, and features.
- Sign with GPG: `git commit -S -F /path/to/message.txt`
- Tooling: [`docs/commit-tooling.md`](docs/commit-tooling.md)

## Critical Rules

1. **Never edit generated code** — regenerate via `make rpc` or `make sqlc`.
2. **Never write raw SQL in Go** — add queries to `db/queries/`, use sqlc.
3. **Run `make lint-native` before every commit** (fastest path, no Docker).
   Falls back to `make lint-changed` if you prefer the Docker-based linter.
4. **Run tests before every commit** — see [`docs/testing-guide.md`](docs/testing-guide.md).
5. Use early returns; do not nest error handling.
6. Do not batch actor messages without backpressure.
7. Comments explain WHY and HOW, not WHAT.

## Knowledge Base Map

### Architecture
- [`ARCHITECTURE.md`](ARCHITECTURE.md) — Package layers, dependency graph, key types, patterns
- [`PLANS.md`](PLANS.md) — ExecPlan specification for complex features

### Deep Docs ([`docs/index.md`](docs/index.md) for full catalog)
- [`docs/development_guidelines.md`](docs/development_guidelines.md) — Complete style guide with WRONG/RIGHT examples
- [`docs/clientconn_architecture.md`](docs/clientconn_architecture.md) — Server-side 1:N mailbox connector architecture
- [`docs/dispatch_pipeline.md`](docs/dispatch_pipeline.md) — Mailbox RPC dispatch pipeline (envelope, operator, actor)
- [`docs/layered_testing_guide.md`](docs/layered_testing_guide.md) — Test layering strategy and patterns
- [`docs/ast-grep-guide.md`](docs/ast-grep-guide.md) — AST-level code search and lint rules
- [`docs/structured-logging.md`](docs/structured-logging.md) — Log format, key-value helpers, error levels
- [`docs/testing-guide.md`](docs/testing-guide.md) — Coverage targets, test approaches, pre-commit checklist
- [`docs/commit-tooling.md`](docs/commit-tooling.md) — commit_message.py workflows

### Per-Package Context
Each major package contains a `CLAUDE.md`/`AGENTS.md` with purpose, key types,
relationships, and invariants. Start from [`ARCHITECTURE.md`](ARCHITECTURE.md)
and navigate into the package relevant to your task.

## Code Generation Workflow

1. **Protobuf**: edit `.proto` → `make rpc` → commit generated code separately.
2. **Database**: edit `db/schema/` or `db/queries/` → `make sqlc` → commit separately.
3. **Never edit generated code manually.**

## Submodule

The `client/` directory is a git submodule pointing to `darepo-client`.
Run `make submodule-init` for first-time setup, `make submodule-update` to pull
latest. Commit the updated submodule pointer after updating.

## Dependencies

For local forks, use replace directives:
```shell
go mod edit -replace=IMPORT-PATH@VERSION=FORK-PATH@FORK-VERSION
```
