# rpc/swapclientrpc

## Purpose

Generated protobuf/gRPC service and message definitions for the daemon-side
swap client service (`SwapClientService`). Exposes the swap subsystem — start,
resume, list, get, and subscribe to swap execution state — to local SDK/CLI
clients.

## Key Types

- `SwapClientService` — Six methods:
  - `StartPay` — Start an Ark-to-Lightning payment swap (daemon continues in
    background after returning the swap handle).
  - `StartReceive` — Start a Lightning-to-Ark receive swap (daemon continues
    the claim after the invoice is paid).
  - `ResumeSwap` — Resume one persisted pending swap by payment hash.
  - `ListSwaps` — List persisted swap sessions from daemon-owned store.
  - `GetSwap` — Fetch one swap summary by payment hash.
  - `SubscribeSwaps` — Stream swap summary updates, with optional snapshot
    of existing swaps.
- `SwapDirection` — `SWAP_DIRECTION_PAY` / `SWAP_DIRECTION_RECEIVE`.
- `SwapSettlementType` — `SWAP_SETTLEMENT_TYPE_LIGHTNING` /
  `SWAP_SETTLEMENT_TYPE_IN_ARK`.
- `SwapState` — 15+ states: `CREATED`, `SWAP_CREATED`, `INVOICE_CREATED`,
  `HTLC_EVENT_ACCEPTED`, `VHTLC_FUNDED`, `CLAIM_INITIATED`, `COMPLETED`,
  `EXPIRED`, `FAILED`, `NEEDS_INTERVENTION`, `REFUND_INITIATED`, `REFUNDED`,
  `CANCELED`, `FUNDING_INITIATED`, `WAITING_FOR_CLAIM`.
- `SwapSummary` — Flat summary row: `PaymentHash`, `State`, `Direction`,
  `SettlementType`, `CreatedAt`, `UpdatedAt`, `ErrorMessage`.
- `StartPayRequest/Response` — Pay request: `Invoice`, `MaxFeeSat`.
  Response: `PaymentHash`.
- `StartReceiveRequest/Response` — Receive request: `AmountMsat`,
  `Memo`. Response: `Invoice`, `PaymentHash`.
- `SubscribeSwapsRequest/Response` — Optional `IncludeExisting` flag for
  initial snapshot.

## Relationships

- **Depends on**: nothing (pure proto-generated types).
- **Depended on by**: `sdk/swaps` (client-side swap orchestration),
  `swapwallet` (daemon-side RPC handler), `cmd/darepocli/darepoclicommands`
  (swap CLI commands), `devrpc` (dynamic CLI).

## Invariants

- **Never edit generated code** — regenerate via `make rpc`.
- Proto source: `rpc/swapclientrpc/swap_client.proto`.

## Deep Docs

- [sdk/swaps/CLAUDE.md](../../sdk/swaps/CLAUDE.md) — High-level swap SDK.
- [swapwallet/CLAUDE.md](../../swapwallet/CLAUDE.md) — Daemon-side handler.
- [rpc/CLAUDE.md](../CLAUDE.md) — Parent rpc package overview.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
