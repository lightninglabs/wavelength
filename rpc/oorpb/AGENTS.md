# rpc/oorpb

## Purpose

Generated protobuf stubs for OOR (Out-of-Round) wire messages exchanged over
the mailbox transport. Do not edit — regenerate via `make rpc`.

## Key Types

- `OORRejectCode` — Discriminated rejection code for server-side OOR
  rejections, allowing clients to branch on the cause without string-matching.
- `oorwire.pb.go` — Generated OOR message types.
- `oorwire_grpc.pb.go` — Generated gRPC stubs.
- `oorwire_mailboxrpc.pb.go` — Generated mailbox-transport bindings for OOR
  messages.
- `payloads.go` — Hand-written helpers for OOR payload construction (the only
  non-generated file in this package).

## Relationships

- **Depends on**: `google.golang.org/protobuf` (proto runtime).
- **Depended on by**: `oor` (uses OOR wire message types), `darepod` (wires
  OOR event routing), `serverconn` (transport delivery).

## Invariants

- `oorwire.pb.go`, `oorwire_grpc.pb.go`, and `oorwire_mailboxrpc.pb.go` are
  generated. Never edit them manually.
- `payloads.go` is hand-written and may be edited directly.
- Regenerate generated files via `make rpc`.

## Deep Docs

- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
