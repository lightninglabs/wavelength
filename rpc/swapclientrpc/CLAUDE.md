# rpc/swapclientrpc

## Purpose

Generated gRPC/REST/mailbox-RPC stubs for `SwapClientService`, the
daemon-owned Lightning/Ark swap execution API (quote/start pay, receive,
credit funding/redemption/listing). Registered only in swapruntime builds.

## Key Types

- `SwapClientServiceClient` / `SwapClientServiceServer` — standard gRPC
  client/server interfaces.
- `SwapClientServiceMailboxServer` — durable-mailbox transport binding.
- Request/response messages (`QuotePayRequest`, `StartPayRequest`,
  `CreateCreditRequest`, `ListCreditsRequest`, etc.) and enums
  (`SwapState`, `SwapDirection`, `CreditOperationState`, ...).

## Relationships

- **Depended on by**: `swapclientserver` (implements the server), `swapwallet`
  (constructs/normalizes RPC types), `cmd/darepocli` (CLI + MCP bindings),
  `rpc/restclient`, `sdk/walletdk`.

## Invariants

- Generated from `swap_client.proto` via `make rpc`; do not hand-edit any
  `.pb.go` / `.pb.gw.go` file.

## Deep Docs

- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map
