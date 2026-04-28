# sdk/swaps

## Purpose

Lightning↔Ark swap orchestration client. Manages durable swap sessions in
both pay (Ark→Lightning) and receive (Lightning→Ark) directions, coordinating
with a swap server via `swaprpc` and the Ark daemon via `sdk/ark`. Session
state is persisted in SQLite so sessions survive daemon restarts.

## Key Types

- `SwapClient` — Main coordinator. Created via `NewSwapClient` (in-memory
  invoice store) or `NewSwapClientWithStore` (persistent store). Drives
  `PaySession` and `ReceiveSession` state machines.
- `SwapDirection` — `SwapDirectionPay` (Ark→Lightning) or
  `SwapDirectionReceive` (Lightning→Ark).
- `SwapSummary` — Stable list view of a persisted swap session (ID,
  direction, state, amounts).
- `PayResult` / `ReceiveResult` — Success outcomes of `PayViaLightning` /
  `ReceiveViaLightning` calls.
- `PaySession` / `ReceiveSession` — Per-session state machine drivers for pay
  and receive flows respectively.
- `PayState` / `ReceiveState` — State enums driving each session FSM.
- `InvoiceCreator` — Interface for creating signed Lightning invoices;
  implementations include `InvoiceGenerator` (keyed) and
  `NewEphemeralInvoiceGenerator` (ephemeral key).
- `ReceiveVHTLCInfo` — Script details for a prepared receive session
  (vHTLC pkScript, expiry, amounts).
- `Store` — Persistent session storage backed by SQLite; created via
  `NewSqliteStore`. Includes `RunMigrations` for automatic schema setup.
- `SqliteStoreConfig` / `DefaultSqliteStoreConfig` — Store configuration
  (data dir, DB filename).
- `MemoryInvoiceStore` / `NewMemoryInvoiceStore` — In-memory invoice store
  for testing.
- `VHTLCConfig` / `RouteHint` — HTLC configuration and routing hints passed
  to the swap server.
- `DaemonConn` — Interface over `sdk/ark.Client` methods needed by the swap
  client (block height, receive scripts, OOR send).
- `SwapServerConn` — Interface over `swaprpc` client methods needed by the
  swap client.
- `GRPCSwapServerConn` / `NewGRPCSwapServerConn` — Production
  `SwapServerConn` wrapping a gRPC `SwapServiceClient`.

## Relationships

- **Depends on**: `sdk/ark` (Ark VTXO operations, receive scripts, block
  height), `swaprpc` (swap server gRPC protocol), `db/sqlc` migrations
  (session persistence schema).
- **Depended on by**: `cmd/darepocli/darepoclicommands` (swap CLI commands).

## Invariants

- Swap direction determines FSM path: pay and receive sessions use independent
  state machines and do not share state.
- Session state is persisted before any external RPC is issued; sessions must
  be resumable after a crash via `SwapClient` restart.
- The `DaemonConn.BlockHeight` call is used for HTLC expiry checks; callers
  must wire this to a live daemon connection.
- `RunMigrations` must be called before using `Store`; the schema is
  append-only and idempotent.
- Never edit generated sqlc code in `sdk/swaps/sqlc/` — regenerate via
  `make sqlc`.

## Deep Docs

- [swaprpc/CLAUDE.md](../../swaprpc/CLAUDE.md) — gRPC swap protocol definitions.
- [sdk/ark/CLAUDE.md](../ark/CLAUDE.md) — Ark SDK facade this package depends on.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
