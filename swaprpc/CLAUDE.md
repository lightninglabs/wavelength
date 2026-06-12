# swaprpc

## Purpose

Generated protobuf/gRPC service and message definitions for Lightning-to-Ark
(receive) and Ark-to-Lightning (pay) atomic swap negotiations via virtual HTLCs
(vHTLCs). This is the external swap-server API: clients open channels, create
swaps, and handle acknowledgements through this service.

## Key Types

- `SwapService` — gRPC service with four methods:
  - `RequestChannelId` — Allocates a short channel ID and route hint for a
    Lightning-to-Ark receive; returns the vHTLC pubkey and invoice hints.
  - `CreateInSwap` — Initiates an Ark-to-Lightning pay swap against a BOLT-11
    invoice; returns negotiated amounts, fees, server pubkey, vHTLC config,
    expiry, and settlement type.
  - `AuthorizeInSwapRefund` — Signs a cooperative refund spend for a failed
    Ark-to-Lightning swap.
  - `AcknowledgeOutSwapHtlc` — Records that the receiver has durably accepted
    an out-swap HTLC mailbox event; the server does not fund the Ark vHTLC
    until this acknowledgement arrives.
- `SettlementType` — Enum: `SETTLEMENT_TYPE_LIGHTNING` (swap server bridges to
  Lightning network) or `SETTLEMENT_TYPE_IN_ARK` (same-Ark vHTLC settlement
  without bridging to Lightning).
- `VHTLCConfig` — vHTLC timelocks and keys: `RefundLocktime`,
  `UnilateralClaimDelay`, `UnilateralRefundDelay`,
  `UnilateralRefundWithoutReceiverDelay`, `SwapserverPubkey`.
- `SwapMailboxEvent` — Envelope for HTLC events delivered via mailbox; carries
  either an `OutSwapHtlcEvent` (Lightning-backed intercepted HTLC) or an
  `InArkHtlcEvent` (same-Ark vHTLC payment).
- `OutSwapHtlcEvent` — Lightning-backed intercepted HTLC: payment hash, raw
  onion blob, vHTLC config. Delivered after receiver acknowledges durable
  acceptance via `AcknowledgeOutSwapHtlc`.
- `InArkHtlcEvent` — Same-Ark vHTLC event: payment hash, sender pubkey, vHTLC
  config, optional indexed outpoint and amount.
- `RouteHint` — Lightning invoice hop hint (node_id, channel_id, fee rates,
  cltv_expiry_delta).
- `TaprootScriptSignature` — Taproot signature carrier (pubkey, witness_script,
  signature bytes, sighash type).

## Relationships

- **Depends on**: nothing (pure proto-generated types).
- **Depended on by**: `sdk/swaps` (client-side swap orchestration),
  `swapclientserver` (daemon-side swap server connection),
  `rpc/restclient` (HTTP transport).

## Invariants

- **Never edit generated code** — regenerate via `make rpc`.
- Proto source: `swaprpc/swap.proto`.

## Deep Docs

- [sdk/swaps/CLAUDE.md](../sdk/swaps/CLAUDE.md) — High-level swap SDK using
  this package.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
