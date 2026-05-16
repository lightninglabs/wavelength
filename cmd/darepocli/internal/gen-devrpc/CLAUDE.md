# gen-devrpc

## Purpose

Code generator (run via `go generate` or a make target) that walks the
registered proto service descriptors for `DaemonService` and
`SwapClientService` and emits `cmd/darepocli/darepoclicommands/devrpc/registry_generated.go`,
the static registry consumed by the `darepocli dev` dynamic command tree.

## Key Types

- `main` — Collects service descriptors from compiled-in proto files,
  renders Go source via a text template, formats it with `go/format`, and
  writes the output file.
- `serviceData` / `methodData` — Intermediate structs holding service and
  method metadata (full name, Go name, aliases, comments) used during
  template rendering.

## Relationships

- **Depends on**: `daemonrpc` (proto file descriptor for DaemonService),
  `rpc/swapclientrpc` (proto file descriptor for SwapClientService),
  `google.golang.org/protobuf/reflect/protoreflect` (descriptor introspection).
- **Depended on by**: nothing at runtime; this is a build-time tool only.
- **Generates**: `cmd/darepocli/darepoclicommands/devrpc/registry_generated.go`.

## Invariants

- The generator must be re-run after any `.proto` change that adds or renames
  a service or method visible to `darepocli dev`.
- `expectedServices` is a hardcoded allowlist; services not in the list are
  skipped, and missing expected services cause a fatal error so the generator
  fails loudly rather than silently omitting a service.

## Deep Docs

- [ARCHITECTURE.md](../../../../ARCHITECTURE.md) — System-wide package map.
