# tools

## Purpose

Development tool dependencies (`tools.go` for protoc plugins, sqlc, linters).

## Relationships

- **Depends on**: nothing (Go module tool dependencies).
- **Depended on by**: `make rpc`, `make sqlc`, `make lint`.

## Local Linting

- Run `make install-custom-gcl` from the repo root to build a native
  `custom-gcl` binary for the current macOS/Linux host.
- After installation, `make lint-local` and `make lint-changed-local`
  reuse that native binary and load the real `ll` plugin instead of the
  fallback `lll` approximation.
