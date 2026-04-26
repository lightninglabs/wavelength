# rpc/roundpb

## Purpose

Generated gRPC protocol types and service constants for the round service.
Defines the two-phase seal-time fee handshake messages introduced in #270:
client intents (Phase 1) and server-issued per-client quotes (Phase 2).

## Key Types

- `JoinRoundRequest` — Phase-1 client intent: target amounts per output,
  `is_change` marker on the designated change output, boarding inputs, forfeits.
- `JoinRoundQuote` — Phase-2 server response issued when the round seals.
  Contains per-output server-decided amounts, total operator fee,
  `FeeBreakdown`, quote expiry timestamp, and a `quote_id` that clients must
  echo in their accept/reject message to detect staleness across reseals.
- `JoinRoundAccept` / `JoinRoundReject` — Client's explicit Phase-2 response
  (echoes `round_id` + `quote_id`). Server reseals over remaining intents after
  a reject or timeout.
- `VTXOQuote` / `LeaveQuote` — Per-output server-computed amounts (one entry
  per `VTXORequest` / `LeaveRequest` in intent order).
- `FeeBreakdown` — Itemized operator fee: `chain_fee_sat`, `liquidity_fee_sat`,
  `congestion_fee_sat`, fee rate at seal time, and batch size. Used for
  client-side cap validation.
- `QuoteReason` — Rejection reason enum: `QUOTE_OK`, `INSUFFICIENT_RESIDUAL`
  (change residual negative or below dust), `INVALID_CHANGE_DESIGNATION`
  (missing or duplicate `is_change` marker).

## Service Method Constants (service.go)

- `MethodJoinRoundQuote` — Server→client push via durable per-client mailbox
  egress; crash between seal and dispatch redelivers on reconnect.
- `MethodAcceptQuote` / `MethodRejectQuote` — Client→server explicit
  accept/reject of the seal-time quote.

## Relationships

- **Depends on**: nothing (generated proto types + handwritten `service.go`
  constants).
- **Depended on by**: `serverconn` (client send/receive), `round` (server
  sends `JoinRoundQuote`), `darepod` (service implementation).

## Invariants

- **Never edit generated `.pb.go` files** — regenerate via `make rpc`.
- `quote_id` must be echoed verbatim in `JoinRoundAccept` / `JoinRoundReject`
  so the server can discard stale responses from a previous seal pass.
- Exactly one output in `JoinRoundRequest` may carry `is_change=true`; the
  server rejects intents that violate this with `INVALID_CHANGE_DESIGNATION`.

## Deep Docs

- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
- [docs/fee-change-model.md](../../docs/fee-change-model.md) — Scenario
  catalogue for the seal-time fee handshake.
