# rpc/walletrpc

## Purpose

Generated protobuf stubs for the `WalletService` gRPC subserver, which
exposes a simplified, swap-vocabulary-free wallet API (Send, Recv, List,
Deposit, Balance, Status, SubscribeWallet) to CLI clients and the
walletdk SDK. Do not edit the generated files — regenerate via `make rpc`.

## Key Types

- `WalletServiceClient` / `WalletServiceServer` — Generated gRPC client and
  server interfaces for the wallet subserver.
- `EntryKind` — Enum tagging each `WalletEntry` with a user-visible category
  (SEND, RECV, EXIT, DEPOSIT, etc.).
- `WalletEntry` — Flat history record projected from swap, OOR, boarding, and
  exit events.
- `wallet.pb.go` — Generated message types.
- `wallet_grpc.pb.go` — Generated gRPC service stubs.
- `wallet_mailboxrpc.pb.go` — Generated mailbox-transport bindings.

## Relationships

- **Depends on**: `google.golang.org/protobuf`.
- **Depended on by**: `swapwallet` (server-side handler), `sdk/walletdk`
  (wallet RPC client), `cmd/darepocli/darepoclicommands` (wallet CLI
  commands).

## Invariants

- All `*.pb.go` and `*_grpc.pb.go` files are generated. Never edit them.
- The `walletrpc` build tag controls whether the server-side handler
  (`swapwallet`) is compiled and registered. The proto stubs are always
  compiled regardless of the build tag.
- Regenerate via `make rpc`.

## Deep Docs

- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
