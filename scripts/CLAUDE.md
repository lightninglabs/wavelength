# scripts

## Purpose

Build, lint, codegen, and release helper scripts (bash and Python) invoked
from Makefile targets. Covers Go/migration version checks, formatting
(`llformat-files.sh`), protobuf/sqlc codegen wrappers, doc cross-link
checks, commit-message linting, custom-linter build/install, and release
tagging. `check-sample-waved-conf/` and `verify-schema-registry/` are
separate Go tools with their own docs.

## Relationships

- **Depends on**: nothing (shell/Python scripts, no repo package imports).
- **Depended on by**: Makefile targets (`make lint`, `make fmt`,
  `make doc-check`, `make commitmsg-lint`, `make schema-check`,
  `make sample-conf-check`, `make rpc`, `make sqlc`, `make install-custom-gcl`).
