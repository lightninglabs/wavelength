# swaprpc

## Purpose

Generated gRPC stub package for the swap server protocol. Defines the
`SwapService` RPC interface and all message types used by clients to initiate
Lightning-to-Ark (out-swap) and Ark-to-Lightning (in-swap) atomic swaps via
virtual HTLCs (vHTLCs). Proto source: `swap.proto`.

## Key Types

- `SwapServiceClient` / `SwapServiceServer` — gRPC stub interfaces for
  `SwapService`. Implemented by `rpc/restclient.SwapServiceClient` (REST) and
  the remote swap server.
- `SettlementType` — enum: `SETTLEMENT_TYPE_UNSPECIFIED` (0),
  `SETTLEMENT_TYPE_LIGHTNING` (1), `SETTLEMENT_TYPE_IN_ARK` (2).
- `RequestChannelIdRequest/Response` — allocates a short channel ID used as a
  route hint in Lightning invoices for out-swap receives.
- `CreateInSwapRequest/Response` — initiates an Ark-to-Lightning swap: client
  provides invoice + max-fee + vHTLC pubkey; server returns vHTLC config and
  routing info.
- `AuthorizeInSwapRefundRequest/Response` — requests a cooperative refund
  authorization for a failed in-swap, signed by the server with a
  `TaprootScriptSignature`.
- `AcknowledgeOutSwapHtlcRequest/Response` — records the receiver's durable
  acceptance of an out-swap HTLC event. The client sends this after validating
  the event, allowing the server to confirm delivery. Added alongside the
  `ReceiveSession.acknowledgeOutSwapHTLC` flow in `sdk/swaps`.
- `SwapMailboxEvent` — envelope wrapping either an `OutSwapHtlcEvent` (server
  intercepted a Lightning HTLC for this receiver) or an `InArkHtlcEvent`
  (same-Ark vHTLC payment).
- `OutSwapHtlcEvent` — Lightning-backed out-swap: intercepted HTLC parameters
  (payment hash, amount, expiry, onion, vHTLC config).
- `InArkHtlcEvent` — same-Ark vHTLC: direct validation without bridging through
  Lightning (payment hash, amount, sender pubkey, vHTLC config).
- `VHTLCConfig` — vHTLC timelocks and keys: `RefundLocktime`,
  `UnilateralClaimDelay`, `UnilateralRefundDelay`,
  `UnilateralRefundWithoutReceiverDelay`, `SwapServerPubkey`.
- `RouteHint` — Lightning hop hint for invoices (NodeID, ChannelID, fees,
  CLTV expiry delta).
- `TaprootScriptSignature` — externally produced tapscript signature for refund
  authorization.

## Relationships

- **Depends on**: `google/protobuf/timestamp.proto` (generated).
- **Depended on by**: `sdk/swaps` (client stubs), `rpc/restclient`
  (HTTP adapter), `swapclientserver` (via `sdk/swaps`).

## Invariants

- **Never edit generated code** — regenerate via `make rpc`.
- `AcknowledgeOutSwapHtlc` is only called for `SETTLEMENT_TYPE_LIGHTNING`
  sessions; same-Ark (`IN_ARK`) sessions skip the server ACK.
- Proto source: `swaprpc/swap.proto`. Generated files: `swap.pb.go`,
  `swap_grpc.pb.go`, `swap.pb.gw.go`, `swap_mailboxrpc.pb.go`.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
- [sdk/swaps/CLAUDE.md](../sdk/swaps/CLAUDE.md) — Client SDK consuming this
  protocol.
