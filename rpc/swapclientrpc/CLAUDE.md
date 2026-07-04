# rpc/swapclientrpc

## Purpose

Generated protobuf/gRPC/REST-gateway/mailbox-RPC stubs for
`SwapClientService` — the local daemon-owned API for Lightning/Ark swap
execution (quote/pay, receive, credit account funding/redemption/listing,
swap listing/resumption/subscription). The service is registered only in
swapruntime builds. Proto source: `rpc/swapclientrpc/swap_client.proto`;
REST gateway annotations in `swap_client.yaml`.

## Key Types

Purely generated, no hand-written files. Every `.go` file
(`swap_client.pb.go`, `swap_client.pb.gw.go`, `swap_client_grpc.pb.go`,
`swap_client_mailboxrpc.pb.go`) is produced by `make rpc` — never edit
directly; regenerate from the `.proto`/`.yaml` sources.

## Relationships

- **Depends on**: nothing (proto definitions).
- **Depended on by**: `swapclientserver` (implements the service),
  `swapwallet` (consumes generated clients for wallet-facing history,
  monitoring, and normalization flows), `sdk/walletdk`, `sdk/swaps`,
  `cmd/darepocli/darepoclicommands` (CLI + MCP swap commands), `darepod`
  (RPC auth wiring), `rpc/restclient` (REST transport).

## Invariants

- **Never edit generated code** — regenerate via `make rpc`.

## Deep Docs

- [rpc/CLAUDE.md](../CLAUDE.md) — Parent rpc package overview.
- [swapclientserver/CLAUDE.md](../../swapclientserver/CLAUDE.md) —
  Server-side implementation of this service.
- [swapwallet/CLAUDE.md](../../swapwallet/CLAUDE.md) — Wallet-facing
  consumer of the generated clients.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
