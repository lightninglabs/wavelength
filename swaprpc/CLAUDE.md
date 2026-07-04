# swaprpc

## Purpose

Generated protobuf/gRPC/REST-gateway/mailbox-RPC definitions for
`SwapService` — the wire protocol between a darepo client and the swap
server: in/out swap negotiation, credit account funding/redemption/
listing, HTLC forfeit signature exchange, and channel-id requests. Proto
source: `swaprpc/swap.proto`; REST gateway annotations in `swap.yaml`.

## Relationships

- **Depends on**: nothing (proto definitions).
- **Depended on by**: `swapclientserver` (implements client-side calls
  into the swap server), `sdk/swaps` (gRPC/REST/mailbox connection
  helpers), `rpc/restclient` (REST transport).

## Invariants

- **Never edit generated code** — regenerate via `make rpc`.

## Deep Docs

- [swapclientserver/CLAUDE.md](../swapclientserver/CLAUDE.md) — Client-side
  consumer that drives swap negotiation over this protocol.
- [sdk/swaps/CLAUDE.md](../sdk/swaps/CLAUDE.md) — Connection helpers for
  gRPC/REST/mailbox transports built on these stubs.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
