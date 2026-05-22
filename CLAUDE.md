# Darepo Agent Guide

This file is a **map**, not a manual. Follow links for details.

## Quick Commands

| Command | Purpose |
|---------|---------|
| `make build` | Compile the project |
| `make lint-local` / `lint-changed-local` | Full / changed-only lint, no Docker |
| `make lint-changed` | Linter on changes vs base (must pass before commit) |
| `make install-custom-gcl` | Build native `custom-gcl` for this host |
| `make fmt` / `fmt-changed` / `fmt-changed-check` | Format all / changed / verify |
| `make unit pkg=<pkg> timeout=5m` | Run unit tests |
| `make unit-debug log="stdlog trace" pkg=<pkg> case=<test>` | Unit tests with debug logs |
| `make itest icase=<test>` | Integration test |
| `make systest` | System-level end-to-end tests |
| `make tidy-module-check` | Verify module files are tidy |
| `make rpc` / `make sqlc` | Regenerate protobuf / type-safe DB queries |
| `make ast-lint` | Check ast-grep style rules |
| `make submodule-update` | Update client submodule to latest |

## Code Style (Summary)

- **8-space tabs** (see `.editorconfig`), 80-char line limit (best effort).
- Every function/method gets a comment starting with its name.
- Exported identifiers need GoDoc comments wrapped to 80 columns.
- Wrap function **calls** with closing `)` on its own line; wrap
  **definitions** with first param on same line, closing `)` with last param.
- Structured logging: `InfoS`/`DebugS`/etc. with static messages and
  `slog.Int()`/`btclog.Fmt()` k-v pairs ([`docs/structured-logging.md`](docs/structured-logging.md)).
- `error` log level is **only** for internal bugs, never external triggers.

Full style guide: [`docs/development_guidelines.md`](docs/development_guidelines.md).

## Git Commits

```
pkg: Short summary in present tense (<=69 chars)

Body wrapped at 72 characters. Explain WHY, not just WHAT.
```

Prefix with package name (`db:`, `rpc:`, `multi:` for multiple). Small atomic
commits — separate fixes, refactors, and features. Tooling:
[`docs/commit-tooling.md`](docs/commit-tooling.md).

## Critical Rules

1. **Never edit generated code** — regenerate via `make rpc` or `make sqlc`.
2. **Never write raw SQL in Go** — add queries to `db/queries/`, use sqlc.
3. **Run `make fmt-changed` + `make lint-changed-local` before every commit.**
   `fmt-changed` runs `goimports` + `llformat` on changed handwritten Go.
   Use `make fmt` / `make lint-local` for full-tree scope when needed.
4. **Run tests before every commit** ([`docs/testing-guide.md`](docs/testing-guide.md)).
5. Use early returns; do not nest error handling.
6. Do not batch actor messages without backpressure.
7. Comments explain WHY and HOW, not WHAT.
8. **Durable actor messages MUST use TLV serialization.** Every
   `actor.TLVMessage` must encode as a `tlv.NewStream(...)` of
   `tlv.MakePrimitiveRecord` (or equivalent) fields — never a
   fixed-layout `binary.Write` over anonymous structs. The durable
   mailbox persists payloads across rolling upgrades; TLV records
   tolerate additive field changes, packed-struct encodings break
   replay the moment any field is added/removed/reordered. Canonical
   templates: [`client/baselib/actor/restart.go`](client/baselib/actor/restart.go)
   and [`client/ledger/messages.go`](client/ledger/messages.go) (primitive
   records, `decodeAmountSat` for `uint64`→`int64` sats,
   `decodeFixedBytes` for fixed-size IDs).

## Efficient Code Lookup (save tokens)

Prefer targeted queries over `Read`-ing whole files when you just need a
signature, docstring, or symbol location.

- `go doc pkg` / `go doc pkg.Symbol` / `go doc -all pkg` / `go doc -src
  pkg.Symbol` — Package overview, single-symbol doc, full surface, or full
  Go source. Works on third-party deps too. Drops a `Read` on a 500-line
  file to ~20 lines.
- `gopls definition <file>:<line>:<col>` / `references` / `symbols <file>` /
  `workspace_symbol <query>` — Jump to definition, find callers (semantic,
  not string), list a file's symbols, fuzzy-search exported symbols.
- `gopls check <file>` — Single-file type-check diagnostics; far faster
  than `go build` on large modules.

Rule of thumb: if you only need to *know* something about a symbol, reach
for `go doc` or `gopls`. Only `Read` when you need to *read* the body.

## Knowledge Base Map

### Architecture
- [`ARCHITECTURE.md`](ARCHITECTURE.md) — Package layers, dependency graph, key types, patterns
- [`PLANS.md`](PLANS.md) — ExecPlan specification for complex features

### Deep Docs ([`docs/index.md`](docs/index.md) for full catalog)
- [`docs/development_guidelines.md`](docs/development_guidelines.md) — Style guide with WRONG/RIGHT examples
- [`docs/clientconn_architecture.md`](docs/clientconn_architecture.md) — Server-side 1:N mailbox connector
- [`docs/dispatch_pipeline.md`](docs/dispatch_pipeline.md) — RPC dispatch pipeline
- [`docs/layered_testing_guide.md`](docs/layered_testing_guide.md) — Test layering strategy
- [`docs/ast-grep-guide.md`](docs/ast-grep-guide.md) — AST-level code search + lint rules
- [`docs/structured-logging.md`](docs/structured-logging.md) — Log format, k-v helpers, error levels
- [`docs/testing-guide.md`](docs/testing-guide.md) — Coverage, approaches, pre-commit checklist
- [`docs/commit-tooling.md`](docs/commit-tooling.md) — commit_message.py workflows

### Per-Package Context
Each major package has a `CLAUDE.md`/`AGENTS.md` (purpose, key concepts,
relationships, invariants). Start at [`ARCHITECTURE.md`](ARCHITECTURE.md)
and navigate into the relevant package.

### Docker Development
- [`docker-compose.yml`](docker-compose.yml) — Full regtest stack (bitcoind + 2× lnd + arkd + darepod).
- [`scripts/docker-regtest-setup.sh`](scripts/docker-regtest-setup.sh) — Setup script.
- [`Dockerfile`](Dockerfile) (server) / [`client/Dockerfile`](client/Dockerfile) (client).

## Code Generation Workflow

1. **Protobuf**: edit `.proto` → `make rpc` → commit generated code separately.
2. **Database**: edit `db/schema/` or `db/queries/` → `make sqlc` → commit separately.
3. **Never edit generated code manually.**

## Submodule

`client/` is a git submodule pointing to `darepo-client`.
`make submodule-init` for first-time, `make submodule-update` for latest.
Commit the updated submodule pointer after updating.

## Dependencies

For local forks:
```shell
go mod edit -replace=IMPORT-PATH@VERSION=FORK-PATH@FORK-VERSION
```
