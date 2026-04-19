# cmd/protoc-gen-mailboxrpc

## Purpose

Protoc plugin that generates typed mailbox RPC stubs from protobuf service
definitions, producing client wrapper types, server interfaces, and router
registration helpers for RPC-over-mailbox communication.

## Key Types

- `Config` (internal/gen) — Plugin configuration; `ExcludeService` skips a
  named service (e.g. `"mailbox.v1.MailboxService"`) during generation.
- `serviceData` / `methodData` (internal/gen) — Template input structs built
  from `protogen.Service`/`protogen.Method`; feed `serviceTmpl`.
- `serviceTmpl` (internal/gen) — `text/template` that emits, per service:
  a typed `*MailboxClient` struct with per-method wrappers calling
  `RPCClient.SendRPC`/`AwaitRPC`, a `*MailboxServer` interface, and a
  `Register*MailboxServer` router helper.

## Relationships

- **Depends on**: `google.golang.org/protobuf/compiler/protogen` (codegen API),
  `mailbox/rpc` (RPCClient, RPCOptions, ServiceMethod, Router — imported only
  in generated output, not in the plugin binary itself).
- **Depended on by**: `make rpc` build pipeline (invoked by protoc via plugin
  protocol); generated files land in `arkrpc/`.

## Invariants

- Output filename is `<prefix>_mailboxrpc.pb.go`; never edit generated files
  directly — re-run `make rpc`.
- Routing keys in generated code are the fully-qualified proto service name
  (e.g. `"arkrpc.ArkService"`) plus the proto method name — these must match
  the server-side router registration.
- `ExcludeService` is matched against the fully-qualified proto service name.

## Deep Docs

- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
