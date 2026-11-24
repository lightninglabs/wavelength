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
- `SwapSettlementType` — Identifies the completed settlement rail. In addition
  to `LIGHTNING`, `IN_ARK`, `CREDIT`, and `MIXED`, `ARK_CHANNEL` means a
  receive completed through an Ark-backed native Lightning channel.
- `SwapSummary` — Flat durable view of one swap. Receive summaries expose
  the existing `channel_id` reserved by an `active_channel_only` request and
  `reserved_scid` for the virtual SCID advertised in the invoice. Before
  exposing a channel-only invoice, clients must check that the response has
  a channel ID: older daemons may ignore the request flag.

## Relationships

- **Depended on by**: `swapclientserver` (implements the server), `swapwallet`
  (constructs/normalizes RPC types), `cmd/wavecli` (CLI + MCP bindings),
  `rpc/restclient`, `sdk/wavewalletdk`.

## Invariants

- Generated from `swap_client.proto` via `make rpc`; do not hand-edit any
  `.pb.go` / `.pb.gw.go` file.
- `SwapSummary` field 29 is reserved under the retired name
  `channel_backing_fee_sat`. Reserved field numbers and names are part of the
  wire contract and must never be reused.

## Deep Docs

- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map
