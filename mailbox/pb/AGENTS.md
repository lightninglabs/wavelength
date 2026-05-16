# mailbox/pb

## Purpose

Generated protobuf stubs for the `MailboxService` gRPC interface — the
bidirectional mailbox transport used between client and Ark server. Do not
edit — regenerate via `make rpc`.

## Key Types

- `MailboxServiceClient` / `MailboxServiceServer` — Generated gRPC client
  and server interfaces for the bidirectional mailbox stream.
- `Envelope` / `RpcMeta` — Core wire types: `Envelope` carries an opaque
  payload with routing headers; `RpcMeta` carries method, correlation ID,
  and kind (request/response/push).
- `mailboxrpc.pb.go` — Generated message types.
- `mailbox_grpc.pb.go` — Generated gRPC service stubs.

## Relationships

- **Depends on**: `google.golang.org/protobuf` (proto runtime).
- **Depended on by**: `serverconn` (uses `MailboxServiceClient` for the
  egress/ingress transport), `sdk/swaps` (uses `MailboxServiceClient` for
  the out-swap event mailbox pull).

## Invariants

- All files are generated. Never edit them manually.
- Regenerate by running `make rpc` after changing `mailbox/pb/mailbox.proto`.

## Deep Docs

- [docs/mailbox_architecture.md](../../docs/mailbox_architecture.md) —
  Three-layer mailbox system (pb, rpc, conn, serverconn).
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
