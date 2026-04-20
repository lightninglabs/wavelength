# cmd/protoc-gen-mailboxrpc

## Purpose

`protoc` plugin that generates mailbox-RPC Go stubs from protobuf service
definitions. Each service method gets a typed wrapper that sends requests and
receives responses over the durable mailbox transport instead of plain gRPC.

## Key Types

- `main` — Plugin entry point; wires `flag.FlagSet` for the `exclude_service`
  option and delegates to `gen.Generate`.
- `gen.Config` — `ExcludeService string`: fully-qualified proto service to skip
  (e.g., `"mailbox.v1.MailboxService"`).
- `gen.Generate` — Iterates plugin files with services and calls `generateFile`
  for each.

## Relationships

- **Depends on**: `google.golang.org/protobuf/compiler/protogen` (protoc plugin
  framework).
- **Depended on by**: `make rpc` (runs this plugin via `protoc`); generated
  output lives in `mailbox/pb` and other `*pb` packages.

## Invariants

- Generated files are named `*_mailboxrpc.pb.go` and carry `DO NOT EDIT` headers.
- `ExcludeService` must be a fully-qualified proto service name; partial matches
  are not supported.

## Deep Docs

- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
- [docs/mailbox_architecture.md](../../docs/mailbox_architecture.md) — Mailbox
  transport design the stubs target.
