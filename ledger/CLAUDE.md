# ledger

## Purpose

Durable actor that serializes every operator accounting write — round
confirmations, VTXO forfeits, batch sweeps, OOR transfer fees, wallet UTXO
diffs — into the double-entry fee ledger and the wallet UTXO audit log. Mirrors
the client-side `client/ledger` actor (darepo-client#221, #222) so both sides
share TLV shapes, replay-safety strategy, and log-level conventions.

## Chart of Accounts

Every ledger write crosses two of nine accounts seeded by migration
`db/sqlc/migrations/000010_accounting.up.sql`:

- `treasury_wallet` (asset) — on-chain operator funds.
- `deployed_capital` (asset) — capital backing live VTXOs.
- `user_vtxo_claims` (liability) — operator obligation to VTXO holders.
- `boarding_fee_revenue`, `refresh_fee_revenue`, `offboard_fee_revenue`,
  `oor_fee_revenue` (revenue) — per-product fee buckets.
- `mining_fees` (expense) — L1 miner outflow.
- `external_funding` (equity) — operator capital in/out, booked when the
  diff subsystem sees unattributable treasury movement.

`fees/CLAUDE.md` carries the per-helper `(debit, credit)` contract.

## Key Concepts

Use `go doc ledger.<Symbol>` for signatures. Highlights:

- **`LedgerActor`** — Single-consumer receive loop; caches `clock.Clock` and
  the in-memory `utxoTracker` so handlers don't re-option per message.
- **`ActorConfig`** — Required: `DeliveryStore`, `LedgerStore`,
  `TreasuryTracker`. Optional: logger, `Clock` (test pinning), `ActorID`,
  `WalletUTXOLister` + `UTXOAuditStore` (diff subsystem), `BalanceReader`
  (treasury rehydrate), `UTXOSnapshotReader` (snapshot rehydrate),
  `ChainSource` ref (self-subscribes for `BlockEpochMsg`). All required
  fields are checked at `Start` so misconfiguration fails fast.
- **Startup rehydration** — `LedgerBalanceReader` returns signed balances
  (debits add, credits subtract) so `TreasuryTracker.Reseed` reflects DB
  truth before the mailbox opens; without it congestion pricing silently
  resets to zero. `UTXOSnapshotReader` rebuilds the live UTXO set from the
  audit log so post-restart diffs don't miss spends that happened during
  downtime. Reseed only treats `seeded=true` as "fresh install" when
  `CountAuditRows == 0` (an empty live set with prior rows still seeds with
  empty snapshot).
- **`LedgerMsg` variants** (TLV `0x8xxx`, all encode as
  `tlv.MakePrimitiveRecord` streams; `decodeAmountSat`/`decodeCount`/
  `decodeFixedBytes` narrow wire types):
  - `RoundConfirmedMsg` — Capital committed + boarding/refresh splits;
    pre-inserts `FundingOutpoints` as `round_funding` and `ChangeOutpoints`
    as `round_change` for classifier short-circuit. `BoardingNewSat` and
    `RefreshNewSat` partition `TotalVTXOAmountSat` so the handler books
    boarding and refresh liability legs separately.
  - `VTXOsForfeitedMsg` — Books two legs: `refresh_forfeit` (retire user
    claim) and `refresh_fee` (operator share). Same round_id; partial
    unique index uses event_type to distinguish.
  - `SweepCompletedMsg` — Books `round_sweep` (reclaim) and `mining_fee`
    (absolute on-chain cost); without the fee leg, `treasury_wallet`
    drifts behind chain reality. Carries `ConsumedOutpoints`
    (`sweep_consumption`) and `ReturnOutpoints` (`sweep_return`) for
    classifier attribution.
  - `OORFinalizedMsg` — Gated on `fee = input - output > 0` (OOR is free
    today; schema forbids zero-amount entries).
  - `BlockEpochMsg` — Drives per-block wallet UTXO diff when
    `WalletUTXOLister` is configured.
- **UTXO diff** — `utxoTracker` is single-consumer (no mutex). First block
  seeds; subsequent blocks diff prev vs current and write rows. Ledger
  legs are **not** booked here — see invariants. `UTXOClassification`
  values: `deposit`, `withdrawal`, `sweep_return`, `sweep_consumption`,
  `round_funding`, `round_change`, `change` (legacy), `pending`, `unknown`.
- **`ErrInvalidMessage`** — Caller-side validation sentinel (negative
  amounts, unknown types); handlers wrap so the receive loop logs at
  `WarnS` + dead-letters rather than retrying against permanent `CHECK`
  violations.
- **`NewServiceKey()` / `ServiceKeyName`** — Receptionist key for the
  singleton actor.

## Relationships

- **Depends on**: `fees` (`Record*`, `TreasuryTracker`, `LedgerStore`,
  `AccountID`, `LedgerEventType`), `client/baselib/actor` (durable actor +
  TLV codec), `lnd/clock`, `lnd/fn/v2`, `btcd`.
- **Depended on by**: root `darepo` (wires actor, lister, audit store),
  `db` (`LedgerStoreDB` satisfies `fees.LedgerStore` + `BalanceReader`;
  `UTXOAuditStoreDB` satisfies `UTXOAuditStore` + `UTXOSnapshotReader`).
- **Messages** (all `Tell` into the ledger mailbox):
  - ← `rounds`: `RoundConfirmedMsg`, `VTXOsForfeitedMsg`.
  - ← `batchsweeper` (via root): `SweepCompletedMsg`.
  - ← `oor`: `OORFinalizedMsg`.
  - ← `chainsource` (self-subscribed in `Start` via
    `SubscribeBlocksRequest` + `MapBlockEpoch`): `BlockEpochMsg`.
    `Stop` cancels the subscription before draining.

## Invariants

- **All ledger writes serialize through one actor.** The receive loop is the
  only sanctioned producer; concurrent ledger writes are out of contract.
- **Handler failures log `WarnS`, never `ErrorS`.** Malformed payloads, DB
  constraint hits, and transient persistence errors are externally
  triggered. `ErrorS` is reserved for internal bugs.
- **Clock is injected.** Handlers call `a.clk.Now()`; direct `time.Now()`
  in this package is disallowed.
- **OOR fee leg gated on `fee > 0`.** Zero-fee finalizations log only; the
  schema forbids zero-amount entries.
- **Two-phase UTXO diff classifier.** Round and sweep handlers pre-insert
  `wallet_utxo_log` rows with `source_id` (round_id/batch_id) and an
  attributed classification (`round_funding`, `round_change`,
  `sweep_consumption`, `sweep_return`) before the next `BlockEpochMsg`.
  The diff loop writes `classification='pending'` for what it observes;
  `PromotePendingWalletUTXOLog` later promotes still-unattributed pending
  rows to `deposit`/`withdrawal` and books the external_* ledger legs.
  Attributable movements already have their `UNIQUE(hash, index, event)`
  slot filled, so `ON CONFLICT DO NOTHING` skips them — no double-counting.
- **UTXO snapshot replaces only after writes succeed.** A `ListUnspent` or
  audit-insert failure leaves prev intact so the next block retries
  cleanly; `UNIQUE(hash, index, event)` + `ON CONFLICT DO NOTHING`
  backstops dedup.
- **External-fund idempotency keys** follow the 36-byte
  `outpoint_hash || little-endian-index` shape (`outpointKey`) so
  `fees.RecordExternalDeposit/Withdrawal` match the client's
  `exitIdempotencyKey` layout.
- **`WalletUTXOLister` and `UTXOAuditStore` are independently optional.**
  No lister → no diff. Lister + no audit store → tracker advances, audit
  trail disabled.
- **`TreasuryTracker` update runs LAST in every handler.** Ledger legs are
  persisted before the in-memory mutation, so a mid-handler DB failure
  cannot advance the tracker past the persisted ledger. On retry, the DB
  dedupes via the partial unique index; the tracker mutation runs exactly
  once. This is the invariant that keeps tracker state consistent under
  at-least-once mailbox delivery.

## Deep Docs

- [`docs/fee-model.md`](../docs/fee-model.md) — Fee model, chart of accounts,
  per-event debit/credit table.
- [`client/docs/durable_actor_architecture.md`](../client/docs/durable_actor_architecture.md)
  — Durable actor CDC pattern.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide map.
