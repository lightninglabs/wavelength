# swaprpc

## Purpose

Generated gRPC/REST/mailbox-RPC stubs for `SwapService`, the external swap
server API consumed by the client SDK: Lightning<->Ark swaps (in-swap/
out-swap), channel-ID allocation for Lightning-to-Ark receives, and durable
credit funding/redemption/listing. Proto source: `swaprpc/swap.proto`
(REST-gateway rules in `swap.yaml`). Fully generated; no hand-written Go
files in this package.

## Key Types

- `SwapServiceClient` / `SwapServiceServer` — standard gRPC client/server
  interfaces.
- `SwapServiceMailboxClient` / `SwapServiceMailboxServer` — durable-mailbox
  transport bindings (`mailbox/rpc.RPCClient`/`Router`) for the same RPCs.
- `SettlementType` — which path backs a swap: `LIGHTNING`, `IN_ARK`,
  `CREDIT`, or `MIXED` (funded by both a vHTLC and reserved credit).
- `SwapMailboxEvent` — oneof wrapper for server-pushed mailbox events
  (`OutSwapHtlcEvent`, and others as added) delivered outside the
  request/response RPCs.
- `CreditOperationState` / `CreditOperationType` — externally visible FSM
  states/kinds for durable credit funding, pay, redemption, and receive
  operations.
- Request/response messages per RPC (`CreateInSwapRequest/Response`,
  `QuoteInSwapRequest/Response`, `CreateCreditRequest/Response`,
  `RedeemCreditRequest/Response`, `ListCreditsRequest/Response`,
  `AuthorizeInSwapRefundRequest/Response`,
  `AcknowledgeOutSwapHtlcRequest/Response`,
  `SignInSwapForfeitRequest/Response`,
  `SubmitOutSwapForfeitSignatureRequest/Response`,
  `RequestChannelIdRequest/Response`).

## Relationships

- **Depends on**: `mailbox/rpc` (mailbox-RPC runtime types used by the
  generated mailbox client/server), `google.golang.org/grpc`,
  `grpc-ecosystem/grpc-gateway/v2` (REST gateway in `swap.pb.gw.go`).
- **Depended on by**: `sdk/swaps` (`grpc_conn.go`, `out_swap_mailbox.go` —
  typed clients for the swap FSM), `rpc/restclient` (REST transport
  adapter). `swapclientserver` implements `rpc/swapclientrpc`, not this
  package — only its tests import `swaprpc`.

## Invariants

- **Never edit generated code** (`swap.pb.go`, `swap_grpc.pb.go`,
  `swap.pb.gw.go`, `swap_mailboxrpc.pb.go`) — regenerate via `make rpc` after
  editing `swap.proto` or `swap.yaml`.
- `SettlementType.SETTLEMENT_TYPE_UNSPECIFIED` (0) is treated as Lightning
  for backward compatibility with older server responses; do not repurpose
  the zero value.
- `SwapMailboxEvent` is a proto oneof: read the populated variant, don't
  assume `OutSwapHtlcEvent` is the only case as new event kinds are added.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
