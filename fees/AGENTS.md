# fees

## Purpose

Fee computation, treasury utilization tracking, and double-entry ledger
recording for Ark operator economics. Implements the fee model specified in
[`docs/fee-model.md`](../docs/fee-model.md): per-operation fees with
on-chain share, liquidity cost, congestion pricing, and operator margin.

## Key Types

- `Calculator` — Thread-safe fee computer with atomic hot-reload of the
  `Schedule`. Entry points: `ComputeFee` (raw days), `ComputeBoardingFee`
  (on-chain share + margin only, no liquidity fee), `ComputeForfeitFee`
  (applies `δ_min` floor), `MinViableAmount`, `ExitCost`.
- `Schedule` — Immutable fee parameter set (annual rate, margin, congestion
  thresholds, `MinRefreshDeltaBlocks` fee floor, dust policy). Swapped
  atomically via `Calculator.UpdateSchedule`.
- `FeeBreakdown` — Itemized fee result: liquidity, on-chain share, margin,
  total, effective rate, and `BelowMinViable` flag.
- `TreasuryTracker` — Mutex-guarded capital position tracker with three
  buckets: deployed, pending sweep, and wallet balance. Capital transitions:
  wallet → deployed (OnRoundConfirmed), deployed → pendingSweep
  (OnVTXOsForfeited), pendingSweep → wallet (OnSweepCompleted). The pending
  sweep bucket prevents utilization from spiking during the forfeit-to-sweep
  window.
- `LedgerStore` — Interface for persisting double-entry ledger records.
  Implemented by `db.LedgerStoreDB`.
- `LedgerEntry` — Domain-level double-entry record. Fields carry typed
  payloads so callers cannot accidentally swap accounts, amounts, or
  timestamps: `DebitAccount` / `CreditAccount` are `AccountID`, `Amount` is
  `btcutil.Amount`, `EventType` is `LedgerEventType`, and `CreatedAt` is
  `time.Time` (the DB adapter flattens to a Unix stamp at the boundary).
  `SessionID` is the optional 32-byte OOR session identifier, mutually
  exclusive with `RoundID` at the schema layer. `IdempotencyKey` is the
  opaque caller-supplied dedup key consumed by the partial unique index.
- `AccountID` — Typed chart-of-accounts identifier. The seeded set includes
  `treasury_wallet` / `deployed_capital` (assets), `user_vtxo_claims`
  (liability), `boarding_fee_revenue` / `refresh_fee_revenue` /
  `offboard_fee_revenue` / `oor_fee_revenue` (revenue, one per product),
  `mining_fees` (expense), and `external_funding` (equity). Fee revenue is
  split per product so tax reporting and analytics read gross per-product
  numbers directly from account balances.
- `LedgerEventType` — Typed event classifier. Covers the fee-model events
  (`boarding_deposit`, `boarding_fee`, `refresh_forfeit`, `refresh_fee`,
  `refresh_new_vtxo`, `offboard`, `offboard_fee`, `mining_fee`,
  `round_sweep`, `capital_committed`, `oor_transfer`) plus the external
  wallet-movement events (`external_deposit`, `external_withdrawal`) that
  the ledger actor's UTXO diff subsystem books.
- `Record*` helpers — Debit/credit-stamped helpers for each event type.
  Each helper derives an `IdempotencyKey` from its context identifier
  (round ID or session ID) so at-least-once mailbox replay is a silent
  no-op via the partial unique index in
  `db/sqlc/migrations/000010_accounting.up.sql`. External-fund helpers
  (`RecordExternalDeposit`, `RecordExternalWithdrawal`) take an opaque
  caller-supplied key — typically the 36-byte
  `outpoint_hash || outpoint_index` produced by the wallet UTXO diff loop.

## Relationships

- **Depends on**: `lnd/lnwallet/chainfee` (fee rate types),
  `lnd/lntypes` (weight units), `btcutil` (Amount type used throughout
  the fee and ledger APIs).
- **Depended on by**: `db` (`LedgerStoreDB` implements `LedgerStore`),
  `ledger` (durable actor consumes `Record*` helpers and
  `TreasuryTracker`), `rounds` (fee computation during registration),
  root `darepo` (wiring), `systest` (fee assertions),
  `client/round` (fee estimation).

## Invariants

- **Boarding has zero liquidity fee.** `ComputeBoardingFee` must never
  charge a liquidity component — the user brings on-chain BTC, no operator
  capital is locked.
- **Forfeit applies `δ_min` floor.** `ComputeForfeitFee` uses
  `max(remainingBlocks, MinRefreshDeltaBlocks)` so lazy refreshes near
  expiry still pay a minimum liquidity cost. This is a pricing floor, not
  an admission rule.
- **batchSize is normalized to >= 1.** `ComputeFee`, `ComputeBoardingFee`,
  and `MinViableAmount` all clamp batchSize before dividing to prevent
  zero on-chain share from an unset input.
- **Schedule is immutable once created.** Updates produce a new `Schedule`
  swapped atomically via `Calculator.UpdateSchedule`. Never mutate a
  `Schedule` in place.
- **Ledger accounts match the seeded chart of accounts.** The nine
  account constants exported as `Account*` must match the seed rows in
  `db/sqlc/migrations/000010_accounting.up.sql`. A test in `db` walks
  `AllAccounts()` against the seeded chart to catch drift at build time.
- **Ledger event types match the DB catalog.** Use `capital_committed`,
  `round_sweep`, `offboard_fee`, `external_deposit`, and
  `external_withdrawal`. The older `capital_deployed` /
  `capital_reclaimed` / `operator_revenue` names are removed and must not
  be reintroduced.
- **Fee revenue is routed per product.** `RecordBoardingFee` credits
  `boarding_fee_revenue`; `RecordRefreshFee` credits `refresh_fee_revenue`;
  `RecordOffboardFee` credits `offboard_fee_revenue`; `RecordOORTransfer`
  credits `oor_fee_revenue`. Collapsing any of these back into a single
  `operator_revenue` bucket would break gross-per-product reporting.
- **Ledger entries are strictly double-entry.** Every `Record*` function
  debits one account and credits a different account. The sum of all
  account balances must always be zero.
- **Boarding fee debits `deployed_capital`** (fee carved from deposit
  before user claim is created), while **refresh fee debits
  `user_vtxo_claims`** (user's outstanding claim reduced by fee).
- **RoundID and SessionID are mutually exclusive.** The schema's
  `CHECK (round_id IS NULL OR session_id IS NULL)` constraint enforces
  this; helpers stamp only one of the two per event (round-scoped events
  set `RoundID`, OOR-scoped events set `SessionID`, external events leave
  both nil).
- **Record* functions take injected `time.Time`.** No internal
  `time.Now()` calls — callers pass the timestamp for deterministic
  testing.
- **Treasury KMax is stable across forfeits.** Forfeited capital moves to
  `pendingSweepSat`, not removed from `KMax`. This prevents transient
  utilization spikes from triggering incorrect congestion pricing.

## Deep Docs

- [`docs/fee-model.md`](../docs/fee-model.md) — Full fee model
  specification with formulas, worked examples, and accounting tables.
- [`docs/fee-model-explorer.html`](../docs/fee-model-explorer.html) —
  Interactive fee visualization.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
