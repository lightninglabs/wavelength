# Darepo Agent Guide

This file is a **map**, not a manual. Follow links for details.

## Quick Commands

| Command | Purpose |
|---------|---------|
| `make build` | Compile the project |
| `make lint-local` | Run full linter locally, no Docker |
| `make lint-changed-local` | Run local linter on changes vs base |
| `make lint` | Run linter via Docker (CI canonical) |
| `make install-custom-gcl` | Build native `custom-gcl` for this host |
| `make fmt` | Format all Go source files |
| `make fmt-changed` | Format changed Go source files |
| `make fmt-changed-check` | Verify changed Go source files are formatted |
| `make unit pkg=<pkg> case=<test>` | Run unit tests |
| `make unit log="stdlog trace" pkg=<pkg> case=<test>` | Unit tests with debug logs |
| `make systest` | System integration tests (use `db=postgres` for PostgreSQL) |
| `make tidy-module-check` | Verify module files are tidy |
| `make rpc` | Regenerate protobuf stubs |
| `make sqlc` | Regenerate type-safe DB queries |
| `make ast-lint` | Check ast-grep style rules |
| `make commitmsg-lint range="origin/main..HEAD"` | Lint commit messages on the current branch |
| `make systest` | Run system integration tests (sqlite) |
| `make systest db=postgres` | Run system integration tests (postgres) |

## Code Style (Summary)

- **8-space tabs**, 80-char line limit (best effort).
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
- Tooling: [`docs/commit-tooling.md`](docs/commit-tooling.md)

## Critical Rules

1. **Never edit generated code** — regenerate via `make rpc` or `make sqlc`.
2. **Never write raw SQL in Go** — add queries to `db/sqlc/queries/`, use sqlc.
3. **Run `make fmt-changed` before every commit.**
   This applies `goimports` and `llformat` to changed handwritten Go files.
   Use `make fmt` instead when you intentionally need a full-tree format pass.
4. **Run `make lint-changed-local` before every commit.**
   This is the fast local no-Docker changed-code check. Run `make lint-local`
   when you need the full local lint scope.
5. **Run tests before every commit** — see [`docs/testing-guide.md`](docs/testing-guide.md).
6. **Run `make commitmsg-lint range="origin/main..HEAD"` before pushing.**
   CI runs the same check via [`scripts/commit_message.py`](scripts/commit_message.py);
   subjects must be `<package>: <summary>` ≤69 chars and body lines must wrap
   to 72. Use `python3 scripts/commit_message.py reword --commit <sha>` to
   rewrite an offending commit in place.
7. **No underscores in Go test names** — `TestFoo` not `Test_Foo`.
8. Use early returns; do not nest error handling.
9. Do not batch actor messages without backpressure.
10. Comments explain WHY and HOW, not WHAT.

## Review Skills

- [`.claude/skills/context-lifecycle/SKILL.md`](.claude/skills/context-lifecycle/SKILL.md)
  — Review goroutines, timers, callbacks, actor handoffs, and async cleanup
  for context lifetime bugs where short-lived request cancellation is captured
  by work that should be owned by a daemon, actor, registration, or bounded
  cleanup path.

## Efficient Code Lookup (save tokens)

If you only need to *know* something about a symbol (signature, doc,
callers), reach for `go doc` or `gopls` before `Read`-ing the file.

| Tool | Use case |
|------|----------|
| `go doc pkg` | Package surface — every exported identifier + one-liner. Start here. |
| `go doc pkg.Symbol` | Full doc + signature for one symbol (works on third-party deps too). |
| `go doc -all pkg` | All exported symbols' full docs in one shot. |
| `go doc -src pkg.Symbol` | Full Go source of the named symbol. |
| `gopls definition file:line:col` | Use-site → definition. |
| `gopls references file:line:col` | Semantic caller search (better than grep for renames). |
| `gopls symbols <file>` | Top-level symbols in a file with line ranges. |
| `gopls workspace_symbol <query>` | Fuzzy-search exported symbols across the module. |
| `gopls check <file>` | Single-file type-check; faster than `go build`. |

If your harness exposes an `LSP` tool (gopls), prefer the structural
equivalents — `documentSymbol`, `hover`, `goToDefinition`,
`findReferences`, `goToImplementation`, `incomingCalls`,
`outgoingCalls`, `workspaceSymbol` — output is JSON-structured.

## Knowledge Base Map

### Architecture
- [`ARCHITECTURE.md`](ARCHITECTURE.md) — Package layers, dependency graph, key types, patterns
- [`PLANS.md`](PLANS.md) — ExecPlan specification for complex features

### Deep Docs ([`docs/index.md`](docs/index.md) for full catalog)
- [`docs/development_guidelines.md`](docs/development_guidelines.md) — Complete style guide with WRONG/RIGHT examples
- [`docs/durable_actor_architecture.md`](docs/durable_actor_architecture.md) — CDC pattern, durable mailbox lifecycle
- [`docs/durable_actor_quickstart.md`](docs/durable_actor_quickstart.md) — TLVMessage, ActorBehavior, migration checklist
- [`docs/mailbox_architecture.md`](docs/mailbox_architecture.md) — Three-layer mailbox system (pb, rpc, conn, serverconn)
- [`docs/RPC_MAILBOX_CONTRACT.md`](docs/RPC_MAILBOX_CONTRACT.md) — Envelope semantics, ack watermarks
- [`docs/ast-grep-guide.md`](docs/ast-grep-guide.md) — AST-level code search and lint rules
- [`docs/structured-logging.md`](docs/structured-logging.md) — Log format, key-value helpers, error levels
- [`docs/testing-guide.md`](docs/testing-guide.md) — Coverage targets, test approaches, pre-commit checklist
- [`docs/commit-tooling.md`](docs/commit-tooling.md) — commit_message.py workflows
- [`docs/daemon_cli_guide.md`](docs/daemon_cli_guide.md) — darepod/darepocli setup and CLI reference
- [`docs/go_workspace.md`](docs/go_workspace.md) — Multi-module Go workspace setup
- [`docs/policy_arkscript_review_guide.md`](docs/policy_arkscript_review_guide.md) — Policy-first arkscript reviewer guide

### Per-Package Context
Each major package contains a `CLAUDE.md`/`AGENTS.md` with purpose, key types,
relationships, and invariants. Start from [`ARCHITECTURE.md`](ARCHITECTURE.md)
and navigate into the package relevant to your task.

## Code Generation Workflow

1. **Protobuf**: edit `.proto` → `make rpc` → commit generated code separately.
2. **Database**: edit `db/sqlc/migrations/` or `db/sqlc/queries/` → `make sqlc` → commit separately.
3. **Never edit generated code manually.**

## Dependencies

For local forks, use replace directives:
```shell
go mod edit -replace=IMPORT-PATH@VERSION=FORK-PATH@FORK-VERSION
```
