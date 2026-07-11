# cmd/protoc-gen-mailboxrpc

## Purpose

`protoc` plugin that generates typed mailbox-RPC client/server stubs from
protobuf service definitions. For each service in an input `.proto` file the
plugin emits a `*_mailboxrpc.pb.go` file containing a `<Service>MailboxClient`
struct, a `<Service>MailboxServer` interface, and a
`Register<Service>MailboxServer` handler-registration function that wires
methods into a `mailbox/rpc` router.

## Key Types

- `main` (entry point) — Parses the `exclude_service` flag (fully-qualified
  proto service name to skip), declares
  `pluginpb.CodeGeneratorResponse_FEATURE_PROTO3_OPTIONAL` support on the
  `protogen.Plugin`, and delegates to `gen.Generate()` via
  `protogen.Plugin.Run()`.

## Relationships

- **Depends on**: `cmd/protoc-gen-mailboxrpc/internal/gen` (code generation
  logic), `google.golang.org/protobuf/compiler/protogen`.
- **Depended on by**: `make rpc` build target — invoked by `protoc` during
  stub regeneration.

## Invariants

- Never edit the generated `*_mailboxrpc.pb.go` files directly; regenerate via
  `make rpc`.
- The `exclude_service` flag must be the fully-qualified name (e.g.
  `"mailbox.v1.MailboxService"`) to suppress generation for a service whose
  stubs are hand-written.
- The plugin advertises `FEATURE_PROTO3_OPTIONAL` support so `protoc` will
  invoke it for `.proto` files that use `optional` on proto3 fields;
  removing that flag would make protoc reject such inputs for this plugin.

## Deep Docs

- [internal/gen/CLAUDE.md](internal/gen/CLAUDE.md) — Code generation logic.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
