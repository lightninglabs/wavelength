# ast-grep Code Search and Linting Guide

This project uses ast-grep (`sg`) for AST-level code search and style
enforcement.

## Code Search

Prefer ast-grep over grep for structural Go patterns. For simple text search,
grep is fine.

**Pattern examples:**
```bash
# Find all function calls
sg run -p '$FUNC($$$ARGS)' -l go

# Find method calls
sg run -p '$OBJ.$METHOD($$$ARGS)' -l go

# Find error returns
sg run -p 'return $ERR' -l go

# Find struct literals
sg run -p '&$TYPE{$$$FIELDS}' -l go

# Find all function definitions
sg run -p 'func $NAME($$$ARGS)' -l go
```

## Linting Commands

```bash
make ast-lint                    # Check for style issues
make ast-lint pkg=<dir>          # Focus on a directory
make ast-grep-fix                # Auto-fix safe issues
make ast-grep-fix pkg=<dir>      # Fix in a specific directory
sg scan --interactive            # Review fixes one by one
```

## Rules Enforced

### Formatting (`rules/go-formatting.yml`)

| Rule | Description |
|------|-------------|
| `struct-literal-asymmetric-close` | Multi-line struct literals need closing `}` on its own line |
| `func-call-asymmetric-wrap` | Multi-line function calls need symmetric wrapping (excludes log/error calls) |
| `log-error-expanded-form` | Log/error calls should use compact form, not expanded |
| `switch-case-needs-spacing` | Switch cases should be separated by blank lines |
| `select-case-needs-spacing` | Select cases should be separated by blank lines |
| `no-inline-comments` | Comments should be on their own line, not trailing code |

### Function Definitions (`rules/go-func-def.yml`)

| Rule | Description |
|------|-------------|
| `func-def-dangling-param-comma` | Function params should not end with dangling comma |
| `func-def-dangling-return-paren` | Return types should not start on a new line with `(` |

**Note:** Structured logging calls (`InfoS`, `DebugS`, etc.) are correctly
formatted with closing `)` on the same line as the last attribute per the
development guidelines.

See `rules/` directory for full rule definitions.
