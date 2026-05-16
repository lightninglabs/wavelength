# serverconn/hellotestpb

## Purpose

Minimal generated protobuf stubs used exclusively in `serverconn` integration
tests to validate the mailbox transport without depending on production proto
definitions. Do not edit — regenerate via `make rpc` if the proto changes.

## Key Types

- `HelloRequest` / `HelloResponse` — Simple request/response pair used in
  test scenarios.
- `HelloServiceClient` / `HelloServiceServer` — Generated gRPC stubs for the
  test hello service.
- `hello_mailboxrpc.pb.go` — Generated mailbox-transport bindings for the
  hello service.

## Relationships

- **Depends on**: `google.golang.org/protobuf`.
- **Depended on by**: `serverconn` test files only.

## Invariants

- All files are generated. Never edit them manually.
- This package is test-only infrastructure and must not be imported by
  production code.

## Deep Docs

- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
