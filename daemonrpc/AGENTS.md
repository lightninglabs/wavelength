# daemonrpc

## Purpose

Daemon gRPC API definitions for wallet operations and round queries. Proto
source: `daemonrpc/daemon.proto`.

## Relationships

- **Depends on**: nothing (proto definitions).
- **Depended on by**: `darepod` (implements services), `cmd/darepocli` (uses generated clients).

## Invariants

- **Never edit generated code** — regenerate via `make rpc`.
