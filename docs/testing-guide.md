# Testing Guide

## Coverage Requirements

Strive for **near 90% test coverage** where practical.

## Testing Approaches

- **Unit tests**: Core logic, pricing functions, parsing, validation.
- **Property-based tests**: Use `pgregory.net/rapid` for invariants across wide
  input domains.
- **Golden tests**: View rendering, serialization format snapshots.
- **Integration tests**: End-to-end workflows with fake providers.

## Commands

```bash
# Single package
make unit pkg=<package> case=<test> timeout=5m

# Debug with logs
make unit log="stdlog trace" pkg=<package> case=<test>

# System-level end-to-end test
make systest
```

## Pre-Commit Checklist

Before every commit, run:

0. `make tidy-module-check` — verify module files are tidy.
1. `make unit pkg=$pkg case=$case timeout=5m` — run unit tests.
2. `make unit log="stdlog trace" pkg=$pkg case=$case` — run with debug logs.
3. **Check logs carefully:**
   - Verify structured logging format is correct.
   - Ensure no log spam.
   - **No `[ERR]` lines should appear** unless testing error paths.
4. `make systest` — run affected system-level end-to-end tests.

## Test Naming

Use `TestExecTxJoinOuterActorTx` not `TestExecTx_JoinOuterActorTx`. No
underscores in Go test function names.
