# rpc/swapclientrpc

## Purpose

Generated protobuf stubs for the `SwapClientService` gRPC subserver, which
exposes Lightning↔Ark swap management operations to CLI clients and the
walletdk SDK. Do not edit the generated files — regenerate via `make rpc`.

## Key Types

- `SwapClientServiceClient` / `SwapClientServiceServer` — Generated gRPC
  client and server interfaces for the optional swap subserver.
- `SwapDirection` — Enum discriminating pay vs receive swaps.
- `swap_client.pb.go` — Generated message types.
- `swap_client_grpc.pb.go` — Generated gRPC service stubs.
- `swap_client_mailboxrpc.pb.go` — Generated mailbox-transport bindings.

## Relationships

- **Depends on**: `google.golang.org/protobuf`.
- **Depended on by**: `swapclientserver` (server-side handler),
  `cmd/darepocli/darepoclicommands` (swap CLI commands),
  `sdk/walletdk` (swap RPC client), `cmd/darepocli/internal/gen-devrpc`
  (dev command generation).

## Invariants

- All `*.pb.go` and `*_grpc.pb.go` files are generated. Never edit them.
- Regenerate via `make rpc`.

## Deep Docs

- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
