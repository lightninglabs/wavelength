# gen-devrpc

## Purpose

Build-time code generator that produces
`cmd/darepocli/darepoclicommands/devrpc/registry_generated.go`. Inspects the
`daemonrpc` and `swapclientrpc` proto descriptors at compile time, extracts
method metadata and doc comments from the allowlisted `.proto` source files,
and renders a Go source file containing `generatedRegistry()` — the runtime
service/method table consumed by the `devrpc` command tree.

Run via `go generate` or `make rpc`; do not invoke directly in normal
development.

## Behaviour

1. Collect services from `daemonrpc.File_daemon_proto` and
   `swapclientrpc.File_swap_client_proto` descriptors.
2. Parse proto source comments from the two allowlisted `.proto` files
   (`daemonrpc/daemon.proto`, `rpc/swapclientrpc/swap_client.proto`) by
   scanning for `service` / `rpc` keywords and extracting leading `//`
   comment blocks.
3. Generate aliases:
   - Service aliases are hard-coded (`DaemonService` → `daemon`,
     `SwapClientService` → `swapclient`).
   - Method aliases are auto-derived: camelCase → kebab-case with initialism
     normalisation (`VTXO`→`Vtxo`, `OOR`→`Oor`, etc.) before conversion.
4. Render Go source and format it with `go/format`.
5. Write the output to the fixed path
   `cmd/darepocli/darepoclicommands/devrpc/registry_generated.go`.

## Relationships

- **Depends on**: `daemonrpc` and `rpc/swapclientrpc` (proto descriptors),
  `go/format`, standard library.
- **Depended on by**: the generated output is consumed at runtime by
  `cmd/darepocli/darepoclicommands/devrpc`.

## Invariants

- Only `DaemonService` and `SwapClientService` are expected; missing either
  fails generation.
- Only the two allowlisted `.proto` source files are scanned for comments.
  This is an intentional security boundary: the generator walks up the
  directory tree to locate the repo root, but only reads the named files.
- Generated output must be valid Go (enforced by `go/format`); a format
  failure aborts generation.
- The output path is fixed and the file must be committed to the repo as a
  generated artifact.

## Deep Docs

- [cmd/darepocli/darepoclicommands/devrpc/CLAUDE.md](../../darepoclicommands/devrpc/CLAUDE.md)
  — Runtime consumer of the generated registry.
- [ARCHITECTURE.md](../../../../ARCHITECTURE.md) — System-wide package map.
