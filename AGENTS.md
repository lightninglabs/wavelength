# Darepo Agent Guide

This file is a **map**, not a manual. Follow links for details.

## Quick Commands

| Command | Purpose |
|---------|---------|
| `make build` | Compile the project |
| `make lint-native` | Run native linter on branch changes (preferred, no Docker) |
| `make lint-changed-local` | Run native linter on changes vs base (alternative) |
| `make lint` | Run linter via Docker (CI canonical) |
| `make install-custom-gcl` | Build native `custom-gcl` for this host |
| `make fmt` | Format all Go source files |
| `make unit pkg=<pkg> case=<test>` | Run unit tests |
| `make unit log="stdlog trace" pkg=<pkg> case=<test>` | Unit tests with debug logs |
| `make itest icase=<test>` | Integration test |
| `make tidy-module-check` | Verify module files are tidy |
| `make rpc` | Regenerate protobuf stubs |
| `make sqlc` | Regenerate type-safe DB queries |
| `make ast-lint` | Check ast-grep style rules |
| `make systest` | Run system integration tests (sqlite) |
| `make systest db=postgres` | Run system integration tests (postgres) |

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
- Tooling: [`docs/commit-tooling.md`](docs/commit-tooling.md)

## Critical Rules

1. **Never edit generated code** — regenerate via `make rpc` or `make sqlc`.
2. **Never write raw SQL in Go** — add queries to `db/queries/`, use sqlc.
3. **Run `make lint-native` before every commit.**
   This builds and runs the custom linter natively via `go tool` — much
   faster than Docker. Only lints changes on the current branch.
4. **Run tests before every commit** — see [`docs/testing-guide.md`](docs/testing-guide.md).
5. **No underscores in Go test names** — `TestFoo` not `Test_Foo`.
6. Use early returns; do not nest error handling.
7. Do not batch actor messages without backpressure.
8. Comments explain WHY and HOW, not WHAT.

## Efficient Code Lookup (save tokens)

Prefer targeted queries over `Read`-ing whole files when you just
need a signature, docstring, or symbol location. Reading an entire
file to find one type costs orders of magnitude more tokens than
the tools below.

- `go doc pkg` — Package-level overview: every exported identifier
  with its one-line summary. Start here before opening any file in a
  package you haven't touched.
- `go doc pkg.Symbol` — Full doc comment + signature for one type,
  function, constant, or method. Drops a `Read` on a 500-line file to
  ~20 lines of output. Works on third-party dependencies too (e.g.
  `go doc github.com/lightningnetwork/lnd/fn/v2.Option`).
- `go doc -all pkg` — Every exported symbol's full doc in one shot;
  useful when auditing a package's surface.
- `go doc -src pkg.Symbol` — Full Go source of the named symbol.
  Cheaper than opening the file when the surrounding code is
  irrelevant.
- `gopls definition file:line:col` — Jump from a use site to the
  definition when you need the exact file:line (returns
  `filename:line:col-line:col`).
- `gopls references file:line:col` — Find every caller of a function
  or use of a type. Alternative to grepping when you want semantic
  matches instead of string matches.
- `gopls symbols <file>` — List all top-level symbols in a file with
  their kinds and line ranges; useful to navigate a big file without
  reading it end-to-end.
- `gopls workspace_symbol <query>` — Fuzzy-search exported symbols
  across the whole module.
- `gopls check <file>` — Run type-check diagnostics on a single file
  without a full `go build`, which is dramatically faster on large
  modules.

If your harness exposes an `LSP` tool (gopls), the same operations
are available structurally: `documentSymbol`, `hover`,
`goToDefinition`, `findReferences`, `goToImplementation`,
`incomingCalls`, `outgoingCalls`, `workspaceSymbol`. Prefer those
over the CLI when available — output is JSON-structured.

Rule of thumb: if you only need to *know* something about a symbol
(signature, doc, callers), reach for `go doc` or `gopls` first. Only
open a file with `Read` when you actually need to *read* its body.

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
2. **Database**: edit `db/schema/` or `db/queries/` → `make sqlc` → commit separately.
3. **Never edit generated code manually.**

## Dependencies

For local forks, use replace directives:
```shell
go mod edit -replace=IMPORT-PATH@VERSION=FORK-PATH@FORK-VERSION
```
