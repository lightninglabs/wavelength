# swaprpc

## Purpose

Generated protobuf/gRPC stubs for the swap server protocol
(`swaprpc/swap.proto`). Defines the wire types used by `sdk/swaps` to
communicate with the remote swap server for Lightning-to-Ark receive
swaps.

## Key Proto Messages

All `*.pb.go` files are generated — never edit directly; regenerate with
`make rpc`.

Notable messages added in recent proto revisions:

- `AcknowledgeOutSwapHtlcRequest` / `AcknowledgeOutSwapHtlcResponse` —
  Client acknowledges receipt of an out-swap HTLC event to the server
  after durably persisting it. Carries `payment_hash` and
  `client_vhtlc_pubkey`.
- `OutSwapHtlcPart` — Single shard of a multi-path payment (MPP):
  `amount_msat` and `onion_blob`. An `OutSwapHtlcEvent` now carries a
  repeated `parts` field so the server can aggregate multi-part
  Lightning payments into one out-swap event.

## Relationships

- **Depends on**: nothing (proto definitions only).
- **Depended on by**: `sdk/swaps` (uses generated client stubs and
  message types for swap server communication), `rpc/restclient`
  (implements the service interface over HTTP/grpc-gateway).

## Invariants

- **Never edit generated code** — regenerate via `make rpc`.

## Deep Docs

- [sdk/swaps/CLAUDE.md](../sdk/swaps/CLAUDE.md) — Swap SDK using
  these proto types.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
