# ledger

## Purpose

Server-side durable actor that serializes all operator accounting writes —
round confirmations, VTXO forfeits, batch sweeps, OOR transfer fees, and
wallet UTXO diffs — into the double-entry fee ledger and the wallet UTXO
audit log. Produces a crash-safe financial audit trail for treasury
reconciliation, tax reporting, and fee transparency.

The package mirrors the client-side `client/ledger` actor
(lightninglabs/darepo-client#221, #222) so both sides of the wire share
their TLV codec shapes, replay-safety strategy, and log-level
conventions.

## Chart of Accounts

Every ledger write crosses two of the nine accounts seeded by migration
`db/sqlc/migrations/000010_accounting.up.sql`:

- `treasury_wallet` (asset) — on-chain operator funds.
- `deployed_capital` (asset) — operator capital backing live VTXOs.
- `user_vtxo_claims` (liability) — operator's obligation to VTXO holders.
- `boarding_fee_revenue`, `refresh_fee_revenue`, `offboard_fee_revenue`,
  `oor_fee_revenue` (revenue) — fee revenue split per product for
  gross-per-product reporting.
- `mining_fees` (expense) — L1 miner fee outflow.
- `external_funding` (equity) — operator capital contributions /
  withdrawals, booked when the wallet UTXO diff subsystem observes
  unattributable movements in the treasury wallet.

See `fees/CLAUDE.md` for the per-helper `(debit, credit)` contract.

## Key Types

- `LedgerActor` — Durable actor driving all accounting writes through one
  serialized receive loop. Caches the resolved `clock.Clock` at
  construction (`a.clk`) and the in-memory UTXO snapshot (`a.utxo`) so
  handlers do not re-option on every message.
- `ActorConfig` — Optional logger (`fn.Option[btclog.Logger]`), required
  `DeliveryStore`, required `LedgerStore`, the `TreasuryTracker` to
  update on round/forfeit/sweep events, an optional `ActorID` override,
  an optional `Clock` for deterministic tests, optional
  `WalletUTXOLister` / `UTXOAuditStore` that drive the per-block UTXO
  diff subsystem, an optional `BalanceReader` that rehydrates the
  treasury tracker from ledger totals on Start, and an optional
  `UTXOSnapshotReader` that rehydrates the UTXO diff snapshot from the
  audit log on Start.
- `LedgerStore` — Alias for `fees.LedgerStore`; the interface is defined
  there so the `Record*` helpers operate on a package-neutral seam.
- `LedgerBalanceReader` — Startup-time reader that returns the signed
  balance of a chart-of-accounts entry from the persisted ledger (debits
  add, credits subtract). `Start` calls this for `deployed_capital` and
  `treasury_wallet` so `TreasuryTracker` is restored to DB truth before
  the mailbox opens; without it a restart silently resets in-memory
  utilization to zero and suppresses congestion pricing until enough
  round events re-populate the counters. Decoupled from
  `fees.LedgerStore` (write-only by design) so the Record* call surface
  does not grow. Satisfied in production by `db.LedgerStoreDB`.
- `UTXOSnapshotReader` — Startup-time reader that reconstructs the
  treasury wallet's current UTXO set from the persisted audit log
  (every `created` row without a paired `spent` row). `Start` feeds the
  resulting snapshot into `utxoTracker.prev` and flips `seeded=true`
  so the first post-restart block epoch attributes new UTXOs as real
  `external_deposit` / `external_withdrawal` events instead of silently
  folding them into a fresh seeding pass and permanently losing
  attribution for anything that arrived during downtime. Satisfied in
  production by `db.UTXOAuditStoreDB`.
- `ErrInvalidMessage` — Sentinel wrapping caller-side validation failures
  (negative amounts, unknown message types). Handlers wrap this so the
  Receive loop can log at `WarnS` and dead-letter, rather than driving
  infinite nack-and-retry against permanent DB `CHECK` violations.
- `LedgerMsg` — Actor message constraint; implementations must satisfy
  `actor.TLVMessage`. Every variant encodes as a TLV record stream
  (named `tlv.MakePrimitiveRecord` fields, `uint64` satoshi wire type
  narrowed via `decodeAmountSat`, `uint32` count wire type narrowed via
  `decodeCount`, fixed-size IDs validated via `decodeFixedBytes`) so
  rolling upgrades can add fields additively without breaking durable
  mailbox replay. Registered variants (TLV types `0x8xxx`):
  - `RoundConfirmedMsg` — Capital committed, boarding fees, mining fees
    for a confirmed round. Sent by the round subsystem.
  - `VTXOsForfeitedMsg` — Refresh fee collection + capital retirement
    (old user VTXO claim returns to deployed_capital pool) + treasury
    transition (deployed → pendingSweep). Sent by the round subsystem.
    The handler books TWO ledger legs: `refresh_forfeit` (retire the
    user claim) and `refresh_fee` (operator fee share). Both carry the
    same round_id; the partial unique index uses event_type to keep
    them distinct.
  - `SweepCompletedMsg` — Round-sweep reclamation into the treasury
    wallet. Sent by the batch sweeper. Carries an absolute
    `MiningFeeSat` field so the handler books BOTH the `round_sweep`
    reclaim leg and a `mining_fees` leg for the on-chain cost of the
    sweep transaction -- without the second leg, ledger totals drift
    behind on-chain reality by the cumulative sweep fee each cycle.
  - `OORFinalizedMsg` — OOR session finalized. Today OOR is free so the
    handler gates on `input > output`; the plumbing is in place for
    when OOR fees are introduced.
  - `BlockEpochMsg` — New block observed. Drives the per-block wallet
    UTXO diff when `WalletUTXOLister` is configured.
- `WalletUTXOLister` / `WalletUTXO` — Interface and domain type the
  diff subsystem consumes. Concrete wiring to lndbackend lands in a
  follow-up PR; until then the subsystem is inert when None.
- `UTXOAuditStore` / `WalletUTXOLogEntry` — Interface and domain type
  for persisting `wallet_utxo_log` rows. Optional: when None, audit
  writes are skipped but ledger-side accounting still runs.
- `UTXOAuditEvent`, `UTXOClassification` — Typed enums mirroring the
  `utxo_events` and `utxo_classifications` catalog tables seeded by
  migration `000011_utxo_audit_log.up.sql`.
- `utxoTracker` — In-memory snapshot of the treasury wallet UTXO set.
  The first block after startup writes audit rows but skips ledger
  booking (the "seeding" pass); subsequent blocks diff against the
  previous snapshot and book `external_deposit` /
  `external_withdrawal` for unclassified movements. Rehydrated on
  startup by `reseedUTXOSnapshot` when a `UTXOSnapshotReader` is wired,
  so the seeding pass only runs on genuine fresh installs instead of
  on every restart.
- `NewServiceKey()` / `ServiceKeyName` — Typed actor-system service key
  used to resolve the singleton ledger actor from the receptionist.

## Relationships

- **Depends on**: `fees` (Record* helpers, `TreasuryTracker`,
  `LedgerStore`, `AccountID`, `LedgerEventType`),
  `client/baselib/actor` (durable actor framework, TLV codec, service
  keys), `lnd/clock` (injectable time source), `lnd/fn/v2` (Option and
  Result), `btcd` (wire.OutPoint, chainhash).
- **Depended on by**: root `darepo` (wires the actor at startup, feeds
  the lister/audit store, exposes the `LedgerStore` to the RPC layer),
  `db` (`LedgerStoreDB` satisfies `fees.LedgerStore` + the upstream
  `LedgerBalanceReader`; `UTXOAuditStoreDB` satisfies both
  `UTXOAuditStore` and `UTXOSnapshotReader`).
- **Messages to/from** (via `Tell` from producers into the ledger
  mailbox):
  - ← `rounds`: `RoundConfirmedMsg` after VTXOCreatedNotification;
    `VTXOsForfeitedMsg{TotalAmountSat, Count, RefreshFeeSat}` when a
    refresh round forfeits VTXOs -- producers must populate
    TotalAmountSat with the gross forfeited value so the retirement
    leg lands alongside the fee leg.
  - ← `batchsweeper` (via root wiring):
    `SweepCompletedMsg{ReclaimedAmountSat, MiningFeeSat, ...}` on
    expired-VTXO sweep confirmation. Producers must populate
    MiningFeeSat with the absolute on-chain fee paid for the sweep
    transaction (deriving from FeeRateSatVB × vbytes at the producer
    is fine) so the handler can book the mining-fee expense leg.
  - ← `oor` (follow-up PR): `OORFinalizedMsg` after FinalizeAcceptedEvent.
  - ← `chainsource` / `batchwatcher`: `BlockEpochMsg` on each
    connected block.

## Invariants

- **All accounting writes serialize through one actor.** No two goroutines
  ever write ledger rows concurrently; the durable actor's single-consumer
  receive loop is the only sanctioned producer.
- **Handler failures log at `WarnS`, never `ErrorS`.** Malformed payloads,
  DB constraint violations, and transient persistence errors are
  externally triggered; Error-level logging is reserved for internal bugs.
- **Non-positive amounts are rejected with `ErrInvalidMessage`.**
  `validateAmounts` runs at the top of every handler so a bad TLV
  dead-letters cleanly instead of driving infinite retry against the
  SQL `CHECK (amount_sat > 0)` constraint.
- **OOR is free today.** `handleOORFinalized` computes `fee = input -
  output` and only invokes `RecordOORTransfer` when `fee > 0`. Zero-fee
  finalizations are logged but not written (master's schema forbids
  zero-amount entries).
- **Clock is injected, not read.** Handlers call `a.clk.Now()`; direct
  `time.Now()` reads inside the package are disallowed so tests can
  pin timestamps deterministically.
- **UTXO diff seeding skips ledger booking.** The first block after
  startup (or after a `WalletUTXOLister` is first wired) writes audit
  rows but does not emit `external_deposit` ledger entries — those
  UTXOs have prior origin stories elsewhere and double-counting them as
  new external capital would permanently skew the equity account.
- **UTXO diff replaces the snapshot only after writes succeed.** A
  `ListUnspent` error or a persistence failure leaves the previous
  snapshot intact, so the next successful block retries naturally
  without resurrecting already-booked movements.
- **External-fund entries are keyed on outpoint.** `external_deposit` /
  `external_withdrawal` stamp a 36-byte
  `outpoint_hash || little-endian-index` key, matching the client's
  `exitIdempotencyKey` layout so the two sides share a single shape.
- **`WalletUTXOLister` and `UTXOAuditStore` are independently optional.**
  A deployment without a configured lister runs the actor as-is (no UTXO
  diff). A deployment with a lister but no audit store still books
  ledger entries; audit rows are an observability layer, not a
  dependency of accounting correctness.
- **Start rehydrates the TreasuryTracker from the ledger.** When both a
  `TreasuryTracker` and a `LedgerBalanceReader` are configured, `Start`
  calls `GetAccountBalance(AccountDeployedCapital)` + `(AccountTreasuryWallet)`
  and feeds the results into `TreasuryTracker.Reseed` before the mailbox
  opens. The tracker is treated as a projection of the ledger on every
  restart: pendingSweepSat folds into deployedCapital (no separate column
  in the ledger yet) and liveVTXOCount resets to zero (the schema tracks
  amounts, not counts). Subsequent forfeit / sweep events re-establish
  the pending-sweep split and new round events re-accumulate the count.
- **Start rehydrates the UTXO diff snapshot from the audit log.** When
  both a `WalletUTXOLister` and a `UTXOSnapshotReader` are configured,
  `Start` calls `ListLiveWalletUTXOs` and uses the result as the
  post-restart baseline with `seeded=true`. The genuine seeding pass
  runs only when the audit log is empty (fresh install), so a restart
  during normal operation no longer loses attribution for external
  deposits that confirmed during downtime.
- **handleVTXOsForfeited books both a retirement leg and a fee leg.**
  The retirement leg (`refresh_forfeit`, debit user_vtxo_claims, credit
  deployed_capital) books the gross forfeited amount so user_vtxo_claims
  converges to the real outstanding obligation; the fee leg
  (`refresh_fee`, debit user_vtxo_claims, credit refresh_fee_revenue)
  books the operator share. Both share round_id; the partial unique
  index uses event_type to keep them distinct.
- **handleSweepCompleted books both a reclaim leg and a mining-fee
  leg.** The reclaim leg (`round_sweep`, debit treasury_wallet, credit
  deployed_capital) returns capital to the operator; the mining-fee
  leg (`mining_fee`, debit mining_fees, credit treasury_wallet) books
  the absolute on-chain cost of the sweep tx. Without the mining-fee
  leg, treasury_wallet drifts behind on-chain reality by the cumulative
  sweep fee. Both legs share BatchID as the idempotency key;
  event_type differentiates them in the partial unique index.
- **TreasuryTracker update runs LAST in every handler.** Every handler
  writes ledger legs before touching the in-memory tracker so a mid-
  handler DB failure does not advance the tracker ahead of the
  persisted ledger. On retry, the DB deduplicates via the partial
  unique index and the tracker mutation runs exactly once -- this is
  the invariant that keeps tracker state consistent under
  at-least-once mailbox delivery.

## Deep Docs

- [`docs/fee-model.md`](../docs/fee-model.md) — Fee model, chart of
  accounts, per-event debit/credit table.
- [`client/docs/durable_actor_architecture.md`](../client/docs/durable_actor_architecture.md)
  — Durable actor CDC pattern shared with the client side.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
