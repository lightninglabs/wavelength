# fees

## Purpose

Fee computation, treasury utilization tracking, and double-entry ledger
recording for Ark operator economics. Implements the fee model in
[`docs/fee-model.md`](../docs/fee-model.md): per-operation fees with on-chain
share, liquidity cost, congestion pricing, and operator margin.

## Key Concepts

Use `go doc fees.<Symbol>` for signatures.

- **`Calculator`** — Thread-safe fee computer with atomic hot-reload of the
  `Schedule`. Entry points: `ComputeFee` (raw days), `ComputeBoardingFee`
  (on-chain share + margin, no liquidity), `ComputeForfeitFee` (applies
  `δ_min` floor), `MinViableAmount`, `ExitCost`.
- **`Schedule`** — Immutable fee parameters (annual rate, margin,
  congestion thresholds, `MinRefreshDeltaBlocks` fee floor, dust policy).
  Swapped atomically via `Calculator.UpdateSchedule`. `Validate()` rejects
  negative rates, out-of-range percentages, etc., before runtime apply or
  `FeeScheduleStoreDB` persist.
- **`FeeBreakdown`** — Itemized result (liquidity, on-chain share, margin,
  total, effective rate, `BelowMinViable`).
- **`TreasuryTracker`** — Mutex-guarded capital position with three buckets
  (deployed, pending sweep, wallet). Transitions:
  wallet → deployed (`OnRoundConfirmed`), deployed → pendingSweep
  (`OnVTXOsForfeited`), pendingSweep → wallet (`OnSweepCompleted`).
  Pending-sweep prevents utilization spikes during the forfeit-to-sweep
  window. **Projection of the ledger, not an independent accumulator** —
  `Reseed(deployedCapitalSat, pendingSweepSat, liveVTXOCount,
  walletBalance)` overwrites every bucket from authoritative totals on
  every restart. Pending-sweep folds into deployed at reseed (ledger
  doesn't split them yet); forfeit/sweep events re-establish the split as
  traffic flows. Conservative (over-counts pending as deployed) rather
  than silently under-pricing.
- **`LedgerEntry`** — Domain double-entry record with typed payloads:
  `DebitAccount`/`CreditAccount` are `AccountID`, `Amount` is
  `btcutil.Amount`, `EventType` is `LedgerEventType`, `CreatedAt` is
  `time.Time`. The DB adapter flattens at the boundary. `SessionID` is
  the optional 32-byte OOR id (mutually exclusive with `RoundID` at the
  schema layer). `IdempotencyKey` is the opaque dedup key consumed by
  the partial unique index.
- **`AccountID`** (chart of accounts): `treasury_wallet`,
  `deployed_capital` (assets); `user_vtxo_claims` (liability);
  `boarding_fee_revenue`, `refresh_fee_revenue`, `offboard_fee_revenue`,
  `oor_fee_revenue` (revenue, split per product so tax/analytics read
  gross-per-product); `mining_fees` (expense); `external_funding`
  (equity).
- **`LedgerEventType`** — Fee-model events (`boarding_deposit`,
  `boarding_fee`, `refresh_forfeit`, `refresh_fee`, `refresh_new_vtxo`,
  `offboard`, `offboard_fee`, `mining_fee`, `round_sweep`,
  `capital_committed`, `oor_transfer`) plus reserved external-wallet
  events (`external_deposit`, `external_withdrawal`, unused on this
  branch — classifier PR wires producers in).
- **`Record*` helpers** — Per-event debit/credit-stamped helpers. Each
  derives an `IdempotencyKey` from its context id (round/session) so
  at-least-once mailbox replay is a silent no-op via the partial unique
  index in `db/sqlc/migrations/000010_accounting.up.sql`. External-fund
  helpers take an opaque caller-supplied key (will be the 36-byte
  `outpoint_hash || little-endian-index` from the UTXO diff loop).
- **`LedgerStore`** — Interface implemented by `db.LedgerStoreDB`.

## Relationships

- **Depends on**: `lnd/lnwallet/chainfee`, `lnd/lntypes`, `btcutil`.
- **Depended on by**: `db` (`LedgerStoreDB`), `ledger` (consumes
  `Record*` + `TreasuryTracker`), `rounds`, root `darepo`, `systest`,
  `client/round`.

## Invariants

- **Boarding has zero liquidity fee** — user brings on-chain BTC, no
  operator capital is locked.
- **Forfeit applies `δ_min` floor**: `ComputeForfeitFee` uses
  `max(remainingBlocks, MinRefreshDeltaBlocks)` so lazy near-expiry
  refreshes still pay a minimum. Pricing floor, not an admission rule.
- **batchSize normalized to ≥ 1** before dividing in `ComputeFee`,
  `ComputeBoardingFee`, `MinViableAmount` — prevents zero on-chain share.
- **`Schedule` is immutable** — updates produce a new `Schedule` swapped
  atomically via `UpdateSchedule`.
- **Account constants match `000010_accounting.up.sql`** — `db` walks
  `AllAccounts()` against the seed at build time to catch drift.
- **Event type names match the catalog** — `capital_committed`,
  `round_sweep`, `offboard_fee`, `external_deposit`,
  `external_withdrawal`. The older `capital_deployed` /
  `capital_reclaimed` / `operator_revenue` names are removed; do not
  reintroduce.
- **Fee revenue is per product** — `RecordBoardingFee` →
  `boarding_fee_revenue`, `RecordRefreshFee` → `refresh_fee_revenue`,
  `RecordOffboardFee` → `offboard_fee_revenue`, `RecordOORTransfer` →
  `oor_fee_revenue`. Collapsing into a single bucket breaks
  gross-per-product reporting.
- **Strictly double-entry** — every `Record*` debits one account and
  credits a different one; sum of balances must always be zero.
- **Boarding fee debits `deployed_capital`** (fee carved from deposit
  before user claim); **refresh fee debits `user_vtxo_claims`** (user's
  outstanding claim reduced).
- **`RoundID` and `SessionID` are mutually exclusive** — schema enforces
  `CHECK (round_id IS NULL OR session_id IS NULL)`.
- **No internal `time.Now()`** — `Record*` takes injected `time.Time`
  for deterministic testing.
- **Treasury `KMax` is stable across forfeits** — forfeited capital moves
  to `pendingSweepSat`, not subtracted from `KMax`, preventing
  transient utilization spikes from triggering wrong congestion pricing.
- **`Reseed` and `Initialize` are the only paths that set tracker state
  without accumulating** from current values.

## Deep Docs

- [`docs/fee-model.md`](../docs/fee-model.md) — Full fee model spec.
- [`docs/fee-model-explorer.html`](../docs/fee-model-explorer.html) —
  Interactive visualization.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide map.
