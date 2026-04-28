# swaprpc

## Purpose

gRPC swap server protocol definitions and generated Go stubs for the
Lightning↔Ark swap service. Provides both standard gRPC client/server
interfaces and a mailbox-aware server interface for durable RPC routing.

## Key Types

- `SwapServiceClient` / `SwapServiceServer` — Standard gRPC client and server
  interfaces generated from `swap.proto`.
- `SwapServiceMailboxServer` — Mailbox-aware server interface for durable RPC
  routing via the transport layer.
- `RegisterSwapServiceMailboxServer(r rpc.Router, impl SwapServiceMailboxServer)` — Registers mailbox routes for all swap RPCs.
- `RequestChannelIdRequest` / `RequestChannelIdResponse` — Lightning→Ark
  receive initiation: client requests a channel ID (payment hash) so the swap
  server can route an incoming Lightning payment to the client's Ark VTXO.
- `CreateInSwapRequest` / `CreateInSwapResponse` — Ark→Lightning pay
  initiation: client locks an Ark VTXO and the swap server provides a
  Lightning payment route.
- `VHTLCConfig` — Virtual HTLC configuration (preimage hash, expiry, amounts).
- `RouteHint` — Lightning routing hint for reaching the swap server over
  private channels.

## Relationships

- **Depends on**: `mailbox/rpc` (mailbox routing infrastructure), generated
  proto stubs.
- **Depended on by**: `sdk/swaps` (uses `SwapServiceClient` and
  `SwapServiceMailboxServer`).

## Invariants

- **Never edit generated code** — regenerate via the proto compiler
  (`make rpc`). Only `swap.proto` and the registration helper are
  hand-maintained.
- Proto source: `swaprpc/swap.proto`.

## Deep Docs

- [sdk/swaps/CLAUDE.md](../sdk/swaps/CLAUDE.md) — Higher-level swap orchestration.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
