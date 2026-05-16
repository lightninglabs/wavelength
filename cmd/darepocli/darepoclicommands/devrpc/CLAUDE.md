# devrpc

## Purpose

Generated low-level CLI command tree for the `darepocli dev` subcommand.
Dynamically builds cobra commands from the daemon's proto file descriptors so
every gRPC service and method is reachable from the CLI without hand-writing
one command per RPC.

## Key Types

- `NewDevCmd(Config)` — Entry point. Builds the full `dev [service] [call]`
  cobra command tree from the service registry. Returns a usable command even
  when the registry fails to load (the failure surfaces at run time so the
  `--help` path still works).
- `Config` — Wiring configuration for the generated command tree, providing
  the gRPC connection factory used when a method command is executed.
- `registry_generated.go` — Machine-generated service registry produced by
  `cmd/darepocli/internal/gen-devrpc`. Do not edit by hand; regenerate via
  `make rpc` (or the relevant make target).

## Relationships

- **Depends on**: `daemonrpc` (proto descriptors for DaemonService),
  `rpc/swapclientrpc` (proto descriptors for SwapClientService),
  `google.golang.org/protobuf` (dynamic proto reflection and JSON marshalling).
- **Depended on by**: `cmd/darepocli/darepoclicommands` (registers `dev` as a
  subcommand of the root CLI).
- **Sends**: → daemon gRPC endpoint: arbitrary method calls driven by proto
  reflection.
- **Receives**: ← API: CLI invocation with flag-bound proto field values.

## Invariants

- `registry_generated.go` is generated code — never edit it manually.
  Regenerate via the `gen-devrpc` binary (see `cmd/darepocli/internal/gen-devrpc`).
- Field binders are built from proto field descriptors at command
  construction time; flag names match proto field names with hyphens for
  nested fields.
- JSON marshalling uses `google.golang.org/protobuf/encoding/protojson` for
  round-trip-safe output.

## Deep Docs

- [docs/daemon_cli_guide.md](../../../../docs/daemon_cli_guide.md) — CLI reference.
- [ARCHITECTURE.md](../../../../ARCHITECTURE.md) — System-wide package map.
