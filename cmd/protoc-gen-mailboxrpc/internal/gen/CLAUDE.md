# cmd/protoc-gen-mailboxrpc/internal/gen

## Purpose

Code generation engine for the `protoc-gen-mailboxrpc` plugin. Iterates over
protobuf file descriptors and emits typed `mailbox/rpc` client/server stubs for
each non-excluded service, using `text/template` expansion over per-service
metadata.

## Key Types

- `Config` — Plugin configuration: `ExcludeService string` (fully-qualified
  proto service name to skip, e.g. `"mailbox.v1.MailboxService"`).
- `Generate(plugin, cfg)` — Entry point: iterates all proto files in the
  `CodeGeneratorRequest`, calls `generateFile` for each.
- `generateFile(plugin, file, cfg)` — Produces `*_mailboxrpc.pb.go` for one
  proto file if it contains non-excluded services.
- `buildServiceData(g, svc, serviceFQN)` — Constructs the template variables
  (service name, FQN, method list with request/response types, import
  references for `mailbox/rpc`, `context`, `proto`, `fmt`).
- `serviceData` / `methodData` — Template input structs.
- `serviceTmpl` — `text/template` expanding `serviceRawTemplate` into Go
  source that calls the generated `RPCClient`'s `SendRPC`/`AwaitRPC` methods
  and registers handlers with `rpc.Router`.

## Relationships

- **Depends on**: `google.golang.org/protobuf/compiler/protogen`,
  `text/template`; references `mailbox/rpc` import path in generated output
  (not a Go import of this package itself).
- **Depended on by**: `cmd/protoc-gen-mailboxrpc` (parent plugin entry point).

## Invariants

- `shouldGenerateFile` skips files where all services are excluded, preventing
  empty stub files.
- Service routing keys in generated output embed the fully-qualified proto
  service name (`"<package>.<ServiceName>"`) so `mailbox/rpc` router
  lookups are deterministic.
- Tests in `generator_test.go` verify that routing keys contain both the proto
  package and service name, and that `exclude_service` suppresses generation.

## Deep Docs

- [ARCHITECTURE.md](../../../../ARCHITECTURE.md) — System-wide package map.
