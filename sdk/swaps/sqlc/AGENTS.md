# sdk/swaps/sqlc

## Purpose

Generated type-safe SQL query layer for the swap client's isolated
persistence store. Produced by `make sqlc` from query files in
`sdk/swaps/sqlc/queries/`. Uses a separate migration table
(`swap_client_schema_migrations`) so it is completely independent from
the main daemon DB.

## Key Generated Types

- `PaySwap` — Ark-to-Lightning swap row: `PaymentHash`, `Invoice`, `State`,
  `AmountMsat`, `FeeMsat`, `ExpiresAt`, client/operator/server pubkeys,
  vHTLC parameters (`RefundLocktime`, `UnilateralClaimDelay`, etc.),
  `RefundSessionId`, `RecoveryId`, `Preimage`, `InterventionReason`.
- `ReceiveSwap` — Lightning-to-Ark swap row: `PaymentHash`, `AmountMsat`,
  `State`, `Invoice`, `Preimage`, `DeadlineBlockHeight`, client/operator/
  server pubkeys, vHTLC parameters, optional `VhtlcOutpoint`/`VhtlcAmount`,
  `SettlementType`.

## Relationships

- **Depends on**: SQLite driver (`modernc.org/sqlite`); standard library only.
- **Depended on by**: `sdk/swaps` (consumes via `Store` type with optional
  persistence).

## Invariants

- **Never edit generated code** — regenerate via `make sqlc`.
- Swap persistence is optional; `sdk/swaps.NewSwapClient` (no store) and
  `NewSwapClientWithStore` (SQLite-backed) are both valid.

## Deep Docs

- [sdk/swaps/CLAUDE.md](../CLAUDE.md) — Parent swap SDK overview.
- [ARCHITECTURE.md](../../../ARCHITECTURE.md) — System-wide package map.
