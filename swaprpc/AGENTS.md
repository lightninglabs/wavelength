# swaprpc

## Purpose

Generated protobuf stubs for the daemon-internal swap RPC wire protocol used
by `swapclientserver` to communicate with the remote swap server over
mailbox-backed channels. Do not edit — regenerate via `make rpc`.

## Key Types

- `SettlementType` — Enum identifying whether a swap bridges through
  Lightning or settles in-Ark (same Ark instance).
- `swap.pb.go` — Generated message types for swap session negotiation.
- `swap_grpc.pb.go` — Generated gRPC stubs.
- `swap_mailboxrpc.pb.go` — Generated mailbox-transport bindings.

## Relationships

- **Depends on**: `google.golang.org/protobuf`.
- **Depended on by**: `swapclientserver` (uses swap wire types for
  server-to-client swap negotiation messages).

## Invariants

- All files are generated. Never edit them manually.
- Regenerate via `make rpc`.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
