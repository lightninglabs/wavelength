# internal

## Purpose

Internal helpers not importable from outside the module. This includes test
utilities and shared production-only constants that should stay scoped to this
module.

## Sub-Packages

- `internal/indexerlimits` — Shared client-side bounds for indexer pagination cursors.
- `internal/testutils` — Deterministic key pair and Schnorr signature generation for tests.

## Relationships

- **Depends on**: package-specific test helpers only.
- **Depended on by**: internal module packages only.
