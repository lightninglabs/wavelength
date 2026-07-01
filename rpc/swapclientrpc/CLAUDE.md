# rpc/swapclientrpc

## Purpose

Generated gRPC stubs for `swapclientrpc.SwapClientService` — daemon-owned
Lightning↔Ark swap execution exposed to local clients (CLI, SDK, MCP). The
daemon runs swaps as durable background sessions; this service starts them,
resumes them after a restart, and streams/query their state. Registered only
in swapruntime builds. Proto source:
`rpc/swapclientrpc/swap_client.proto`.

## Key Types

- `SwapClientService` — `QuotePay`/`StartPay` (Ark→Lightning payment),
  `StartReceive` (Lightning→Ark receive), `CreateCredit`/`RedeemCredit`/
  `ListCredits` (server-owned sat-native credit account), `ResumeSwap`
  (wake a persisted pending swap), `ListSwaps`/`GetSwap` (query persisted
  sessions), `SubscribeSwaps` (server-streaming swap updates).
- `SwapSummary` — The canonical durable session row returned by
  `StartPay`/`StartReceive`/`ResumeSwap`/`ListSwaps`/`GetSwap`/
  `SubscribeSwaps`. Carries `direction` (`SwapDirection`), `state`
  (`SwapState`), `settlement_type` (`SwapSettlementType`), amounts, the
  observed vHTLC outpoint/amount, correlated OOR session ids
  (`funding_session_id`/`claim_session_id`/`refund_session_id`), and
  `preimage` once known.
- `SwapState` — The client-visible FSM state enum (CREATED →
  FUNDING_INITIATED / VHTLC_FUNDED → WAITING_FOR_CLAIM / CLAIM_INITIATED →
  COMPLETED, or REFUND_INITIATED → REFUNDED, or FAILED /
  NEEDS_INTERVENTION).
- `SwapSettlementType` — LIGHTNING, IN_ARK (same-Ark direct vHTLC), CREDIT
  (paid from reserved server credit, no client vHTLC), or MIXED (vHTLC +
  credit together).
- `QuotePayRequest`/`Response` — Non-binding preview of an Ark→Lightning
  payment (fee, settlement type, `CreditQuote`) with no durable state
  created.
- `CreateCreditRequest`/`Response`, `RedeemCreditRequest`/`Response`,
  `ListCreditsRequest`/`Response`, `CreditQuote`, `CreditOperation`,
  `CreditLedgerEntry` — Credit account lifecycle: fund via Lightning receive
  or Ark top-up (`CreditFundingSource`), track state (`CreditOperationState`:
  CREATED → AWAITING_PAYMENT → CREDITED → RESERVED → ... → REDEEMED/
  RELEASED/EXPIRED/FAILED), redeem back into an Ark output, and list the
  account snapshot plus ledger rows.
- `StartReceiveResponse` — Returns the BOLT-11 `invoice` plus
  `available_credit_sat`/`attached_credit_sat`/`vhtlc_amount_sat` so callers
  can see whether/how much credit was blended into the funded vHTLC.

## Relationships

- **Depends on**: nothing (proto definitions).
- **Depended on by**: `swapwallet` (implements the service server-side;
  registered via `swapwallet/register.go`), `swapclientserver` (also
  implements/bridges the service; `credit_bridge.go` wires credit RPCs to
  the swap-server side), `cmd/darepocli/darepoclicommands` (swap/credit CLI
  and MCP verbs dial this service), `sdk/walletdk` (wraps
  `SwapClientServiceClient` for embedded use), `rpc/restclient`
  (`SwapClientServiceClient` REST wrapper).

## Invariants

- **Never edit generated code** — regenerate via `make rpc`.
- `SwapClientService` is registered only when the daemon is built/run with
  the swapruntime feature enabled; callers must not assume it is always
  present (compare `rpc/walletdkrpc`, which is always available and proxies
  admin-shape operations before the swap runtime is live).
- `SubscribeSwaps` with `include_existing` sends the snapshot before
  registering the live subscription; callers that need a complete current
  view after a reconnect gap should reconcile with `ListSwaps`/`GetSwap`
  rather than trusting the stream alone to have replayed everything.
- `idempotency_key` on `StartPayRequest`/`StartReceiveRequest` is reserved
  for a future daemon-level duplicate-start guard; today, duplicate-start
  protection relies on the persisted swap state keyed by payment hash once
  it is known, so callers should not assume the key alone prevents a
  double-start before the payment hash exists.
- `max_credit_sat` gates how much reserved credit a pay/receive is allowed
  to consume; a zero value on `QuotePayRequest`/`CreateInSwapRequest`-style
  fields means "credit use not authorized," not "unlimited."

## Deep Docs

- [swapwallet/CLAUDE.md](../../swapwallet/CLAUDE.md) — Daemon-side service
  implementation and swap FSM orchestration.
- [rpc/walletdkrpc/CLAUDE.md](../walletdkrpc/CLAUDE.md) — Higher-level
  wallet API that composes this service for the 7 wallet verbs.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
