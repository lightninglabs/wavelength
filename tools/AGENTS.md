# tools

## Purpose

Development tool dependencies (`tools.go` for protoc plugins, sqlc, linters).

## Relationships

- **Depends on**: nothing (Go module tool dependencies).
- **Depended on by**: `make rpc`, `make sqlc`, `make lint`.

## Local Linting

- `make lint-changed-local` — fast no-Docker check against branch changes
  (uses native `custom-gcl` built via `make install-custom-gcl`).
- `make lint-local` — full local scope, no Docker.
- `make lint` — canonical Docker-based linter (matches CI).
