# Darepo Agent Assistant Guide

> **IMPORTANT**: For complete style guidelines with detailed examples, see [`docs/development_guidelines.md`](docs/development_guidelines.md). This file provides a quick reference for AI agents.

## Essential Commands

### Building and Testing
- `make build` - Compile the project
- `make tidy-module-check` - Verify module files are tidy
- `make lint` - Run the linter (must pass before committing)
- `make fmt` - Format all Go source files
- `make clean` - Remove build artifacts

### Code Generation
- `make rpc` - Install protoc plugins and regenerate Go/Python stubs
- `make sqlc` - Regenerate type-safe database queries (after schema/query changes)

### Submodule Management
- `make submodule-init` - Initialize submodules (first-time setup)
- `make submodule-update` - Update submodules to latest commits
- `make submodule-status` - Show detailed submodule status
- `make submodule-check` - Verify submodules are initialized (CI check)
- `make submodule-sync` - Sync submodule URLs from .gitmodules

### Testing Commands
- Single package: `make unit pkg=<package> timeout=5m`
- Debug with logs: `make unit-debug log="stdlog trace" pkg=$pkg case=$case timeout=10s`
- Integration test: `make itest icase=$icase`

## Code Style Quick Reference

**IMPORTANT**: Editors must be configured with **tab = 8 spaces** for correct formatting.

### Function and Method Comments
- **Every function and method** (including unexported ones) must have a comment
  starting with the function/method name
- This includes interface implementation methods (e.g., `MessageType()`, sealed
  interface markers)
- Comments should explain **how/why**, not just what
- Use literate programming style—comments should be additive and insightful
- All exported functions need detailed documentation

### GoDoc for Exported Identifiers
- Any exported identifier (type, const, var, func, method) must have a GoDoc
  comment that starts with the identifier name.
- Exported struct fields must have a GoDoc comment (GoDoc style, starting with
  the field name) and wrapped to 80 columns.
- All GoDoc-style comments must be wrapped to 80 columns.

### Comments for Non-trivial Code
- Any non-trivial code blocks (multi-step algorithms, subtle invariants,
  concurrency/locking, retries/idempotency, tricky encodings) must include
  explanatory comments that describe the “why” and any invariants.
- These explanatory comments should also be wrapped to 80 columns.

**Examples:**
```go
// CORRECT: All methods have comments
// MessageType returns the type of this message.
func (m *ScheduleTimeoutRequest) MessageType() string {
	return "ScheduleTimeoutRequest"
}

// timeoutMsgSealed marks this as implementing the sealed Msg interface.
func (m *ScheduleTimeoutRequest) timeoutMsgSealed() {}

// WRONG: Missing comments
func (m *ScheduleTimeoutRequest) MessageType() string {
	return "ScheduleTimeoutRequest"
}

func (m *ScheduleTimeoutRequest) timeoutMsgSealed() {}
```

### Code Organization and Spacing
- 80-character line limit (best effort)
- Organize code into logical stanzas separated by blank lines
- Add explanatory comments between stanzas
- Spacing between switch/select cases
- When wrapping function calls, put closing paren on its own line with all args on new lines

### ast-grep for Code Search and Linting

This project uses ast-grep (`sg`) for AST-level code search and style enforcement.

**Code Search (prefer over grep for Go patterns):**
- Use `sg run -p 'pattern' -l go` for structural code search
- ast-grep understands Go syntax, so `sg run -p 'func $NAME($$$ARGS)' -l go` finds all functions
- For simple text search, grep is fine; for code patterns, use ast-grep

**Pattern examples:**
- Find all function calls: `sg run -p '$FUNC($$$ARGS)' -l go`
- Find method calls: `sg run -p '$OBJ.$METHOD($$$ARGS)' -l go`
- Find error returns: `sg run -p 'return $ERR' -l go`
- Find struct literals: `sg run -p '&$TYPE{$$$FIELDS}' -l go`

**Linting commands:**
- `make ast-lint` - Check for style issues (use `pkg=<dir>` to focus on a directory)
- `make ast-grep-fix` - Auto-fix safe issues (use `pkg=<dir>` to focus on a directory)
- `sg scan --interactive` - Review fixes one by one

**Rules enforced:**

*Formatting (go-formatting.yml):*
- `struct-literal-asymmetric-close`: Multi-line struct literals need closing `}` on its own line
- `func-call-asymmetric-wrap`: Multi-line function calls need symmetric wrapping (excludes log/error/test assertion calls)
- `log-error-expanded-form`: Log/error calls should use compact form, not expanded
- `switch-case-needs-spacing`: Switch cases should be separated by blank lines
- `select-case-needs-spacing`: Select cases should be separated by blank lines
- `no-inline-comments`: Comments should be on their own line above the code, not inline

*Function definitions (go-func-def.yml):*
- `func-def-dangling-param-comma`: Function params should not end with dangling comma
- `func-def-dangling-return-paren`: Return types should not start on a new line with `(`

**Note:** Structured logging calls (`InfoS`, `DebugS`, etc.) are correctly formatted with closing `)` on the same line as the last attribute per the development guidelines.

See `rules/` directory for full rule definitions.

### Structured Logging
**YOU MUST** use structured log methods (ending in `S`) with static messages:
- First parameter: `context.Context`
- Second parameter: static string (no `fmt.Sprintf`)
- Remaining parameters: key-value pairs using `slog.Int()`, `btclog.Fmt()`, `btclog.Hex()`, etc.
- One key-value pair per line for readability
- Lines can exceed 80 chars for structured logging

Example:
```go
log.InfoS(ctx, "Channel open performed",
	slog.Int("user_id", userID),
	btclog.Fmt("amount", "%.8f", 0.00154))
```

### Error Log Levels
**CRITICAL**: Only use `error` level for **internal errors never expected during normal operation**.
- External triggers (RPC failures, chain backend issues, peer disconnects) should use lower levels (`warn`, `info`, `debug`)
- If a user could cause it, it's not an error-level log

## Git Commit Guidelines

### Commit Message Format
```
pkg: Short summary in present tense (≤50 chars)

Longer explanation if needed, wrapped at 72 characters. Explain WHY
this change is being made and any relevant context, not just WHAT
changed.
```

**Commit message rules**:
- First line: present tense ("Fix bug" not "Fixed bug")
- Prefix with package name: `db:`, `rpc:`, `multi:` (for multiple packages)
- Subject ≤50 characters
- Body wrapped at 72 characters
- Blank line between subject and body

### Commit Granularity
**IMPORTANT**: Prefer small, atomic commits that build independently.

Separate commits for:
- Bug fixes (one fix per commit)
- Code restructuring/refactoring
- File moves or renames
- New subsystems or features
- Integration of new functionality

### Commit Signing
Sign commits with GPG when possible: `git commit -S -m "message"`

## Testing Philosophy

### Coverage Requirements
Strive for **near 90% test coverage** where practical.

### Testing Approaches
- **Unit tests**: Core logic, pricing functions, parsing, validation
- **Property-based tests**: Use `pgregory.net/rapid` for invariants across wide input domains
- **Golden tests**: View rendering, serialization format snapshots
- **Integration tests**: End-to-end workflows with fake providers

### Before Committing
**YOU MUST** run tests before every commit:

0. Run module tidy check: `make tidy-module-check`
1. Run unit tests: `make unit pkg=$pkg timeout=5m`
2. Run with debug logs: `make unit-debug log="stdlog trace" pkg=$pkg case=$case timeout=10s`
3. **Check logs carefully**:
   - Verify structured logging format is correct
   - Ensure no log spam
   - **No `[ERR]` lines should appear** unless testing error paths
4. Run affected integration tests: `make itest icase=$icase`

## Development Workflow

### ExecPlans for Complex Features
When implementing significant features or refactors, create an **ExecPlan** following `PLANS.md`:
- Fully self-contained (novice can implement without prior knowledge)
- Living document updated as progress is made
- Must include: Progress checklist, Surprises & Discoveries, Decision Log, Outcomes & Retrospective

### Code Generation Workflow
1. **Protobuf changes**: Edit `.proto` files → run `make rpc` → commit generated code separately
2. **Database changes**: Edit `db/schema/` or `db/queries/` → run `make sqlc` → commit generated code separately
3. **Never edit generated code manually** - Always regenerate via make targets

### Submodule Workflow
The `client/` directory is a git submodule pointing to the `darepo-client` repository.

**First-time setup:**
```bash
make submodule-init
```

**Updating to latest commits:**
```bash
make submodule-update  # Fetches and updates to latest remote commits
git add client         # Stage the submodule pointer update
git commit -m "client: update submodule to latest"
```

**Checking status:**
```bash
make submodule-status  # Shows commit, branch, and any uncommitted changes
```

**Important notes:**
- The submodule uses SSH authentication (`git@github.com`)
- Submodules are automatically checked in CI via `make submodule-check`
- After `submodule-update`, commit the updated submodule pointer
- Run `make submodule-sync` if .gitmodules URLs change

## Important Conventions

### Dependencies
For local forks, use replace directives:
```shell
go mod edit -replace=IMPORT-PATH@VERSION=FORK-PATH@FORK-VERSION
```

See `docs/development_guidelines.md` for full dependency management details.

## Common Pitfalls to Avoid

1. **Do not edit generated code** - Regenerate via `make rpc` or `make sqlc`
2. **Do not write raw SQL in Go** - Add queries to `db/queries/` and use sqlc
3. **Do not use `error` log level for expected failures** - External events use lower levels
4. **Do not skip tests** - All new code requires test coverage
5. **Do not use 4-space tabs** - Configure editor for 8-space tabs
6. **Do not nest error handling** - Use early returns and check errors immediately
7. **Do not batch actor messages without backpressure** - Follow quote coalescing patterns
8. **Do not commit without running `make lint`** - Linter must pass
9. **Do not write comments that restate code** - Comments explain WHY and HOW, not WHAT

## Additional Resources

- **[`docs/development_guidelines.md`](docs/development_guidelines.md)** - Complete style guide with extensive WRONG/RIGHT examples
- **`PLANS.md`** - ExecPlan specification for complex features
- **`.editorconfig`** - Automatic editor configuration (8-space tabs, 80-char lines)
