# tools

## Purpose

Development tool dependencies (`tools.go` for protoc plugins, sqlc, linters).

## Relationships

- **Depends on**: nothing (Go module tool dependencies).
- **Depended on by**: `make rpc`, `make sqlc`, `make lint`.

## Local Linting

- **Preferred**: `make lint-native` builds the custom linter via
  `go tool golangci-lint custom` and runs it on branch changes. No Docker
  required, loads the real `ll` plugin.
- **Alternative**: `make install-custom-gcl` builds a native `custom-gcl`
  binary, then `make lint-changed-local` uses it.
- Both native paths are much faster than `make lint` (Docker) on macOS.
