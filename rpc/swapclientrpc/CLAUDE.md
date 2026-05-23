# rpc/swapclientrpc

## Purpose

Generated gRPC stubs for `SwapClientService` — the daemon-side RPC surface
for Lightning/Ark atomic swaps. Exposes pay and receive swap lifecycle
management to local CLI and SDK clients. Built only under the `swapruntime`
build tag; server-side implementation lives in `swapclientserver`.

Proto source: `rpc/swapclientrpc/swap_client.proto`.

## Service Methods

| Method | Purpose |
|--------|---------|
| `StartPay` | Start an Ark-to-Lightning pay swap; daemon continues in background |
| `StartReceive` | Start a Lightning-to-Ark receive swap; returns the BOLT-11 invoice |
| `ResumeSwap` | Wake a persisted pending swap worker (idempotent) |
| `ListSwaps` | List persisted swap summaries; optionally filter to pending only |
| `GetSwap` | Fetch one persisted swap by payment hash |
| `SubscribeSwaps` | Stream coarse summary updates; optionally emit existing rows first |

## Key Messages

- `SwapSummary` — Durable swap state snapshot: `direction`
  (`SwapDirection`), `payment_hash`, `state` (`SwapState`), `pending`
  flag, amounts (`amount_sat`, `fee_sat`, `max_fee_sat`), vHTLC details
  (`outpoint`, `amount`), session IDs (`funding_session_id`,
  `claim_session_id`, `refund_session_id`), `terminal_reason`,
  timestamps (`created_at`, `updated_at`, `deadline`),
  `refund_locktime`, and `invoice` (BOLT-11 payment request associated
  with the swap).
- `SwapDirection` — `SWAP_DIRECTION_PAY` (1) or `SWAP_DIRECTION_RECEIVE` (2).
- `SwapState` — Full lifecycle enum: `CREATED`, `SWAP_CREATED`,
  `INVOICE_CREATED`, `FUNDING_INITIATED`, `VHTLC_FUNDED`,
  `WAITING_FOR_CLAIM`, `CLAIM_INITIATED`, `COMPLETED`, `EXPIRED`,
  `REFUND_INITIATED`, `REFUNDED`, `NEEDS_INTERVENTION`, `FAILED`.
- `StartPayRequest` — `invoice` (BOLT-11), `max_fee_sat` (routing fee
  cap), `idempotency_key` (reserved, returns Unimplemented).
- `StartReceiveRequest` — `amount_sat`, `idempotency_key` (reserved),
  `memo`.
- `SubscribeSwapsRequest` — `include_existing` (emit current rows),
  `pending_only`.

## Relationships

- **Depends on**: nothing (proto definitions).
- **Depended on by**:
  - `swapclientserver` (implements the service server-side).
  - `cmd/darepocli/darepoclicommands` (`swap.*` CLI commands,
    `swapruntime` tag only).
  - `swapwallet` (consumes `ListSwaps` and `SubscribeSwaps` for the
    activity merge and monitor loop).

## Invariants

- **Never edit generated code** — regenerate via `make rpc`.
- The service is registered only in `swapruntime` builds; callers
  built without the tag receive `codes.Unimplemented`.
- `idempotency_key` on `StartPay`/`StartReceive` is reserved and
  always returns `Unimplemented` to guard against accidental
  duplicate-start assumptions.
- `SwapSummary.invoice` carries the BOLT-11 string for display and
  correlation; its payment hash always matches `payment_hash`.

## Deep Docs

- [swapclientserver/CLAUDE.md](../../swapclientserver/CLAUDE.md) —
  Server-side implementation.
- [sdk/swaps/CLAUDE.md](../../sdk/swaps/CLAUDE.md) — Underlying swap
  FSM that the server drives.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
