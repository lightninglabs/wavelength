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
  `DeliveryStore`, required `LedgerStore` (both validated at the top
  of `Start` so a misconfigured actor fails fast at boot instead of
  nil-dereffing on the first message), the `TreasuryTracker` to
  update on round/forfeit/sweep events, an optional `ActorID` override,
  an optional `Clock` for deterministic tests, optional
  `WalletUTXOLister` / `UTXOAuditStore` that drive the per-block UTXO
  diff subsystem, an optional `BalanceReader` that rehydrates the
  treasury tracker from ledger totals on Start, an optional
  `UTXOSnapshotReader` that rehydrates the UTXO diff snapshot from the
  audit log on Start, and an optional `ChainSource` actor ref that the
  `Start` path uses to self-register for block-epoch notifications so
  each connected block enqueues a `BlockEpochMsg` into the actor's
  own mailbox.
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
  (every `created` row without a paired `spent` row). Exposes two
  methods: `ListLiveWalletUTXOs` returns the live set,
  `CountAuditRows` returns the total row count so reseed can
  distinguish a genuine fresh install (no rows ever written) from a
  deployment whose wallet is temporarily empty (rows exist, live
  count is zero right now). `Start` feeds the live snapshot into
  `utxoTracker.prev` and flips `seeded=true` so a post-restart
  diff only emits audit rows for UTXOs that actually moved while
  the daemon was down -- without rehydration, the first post-
  restart block would miss every spend that happened during
  downtime (an empty prev lets the spent-side diff return
  nothing). Satisfied in production by `db.UTXOAuditStoreDB`.
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
  - `RoundConfirmedMsg` — Capital committed, boarding fees, mining fees,
    and UTXO attribution for a confirmed round. Sent by the round subsystem.
    Carries `FundingOutpoints` (treasury-wallet UTXOs spent as round inputs,
    pre-inserted as `round_funding`) and `ChangeOutpoints` (round outputs
    returning to wallet, pre-inserted as `round_change`), plus `BoardingNewSat`
    and `RefreshNewSat` that partition `TotalVTXOAmountSat` by origin so
    the handler can book `RecordBoardingDeposit` and `RecordRefreshNewVTXO`
    liability legs separately without reintroducing a combined `operator_revenue`
    bucket. The outpoint slices allow the UTXO diff classifier to
    short-circuit external_* booking for round-attributable wallet movements.
  - `VTXOsForfeitedMsg` — Refresh fee collection + capital retirement
    (old user VTXO claim returns to deployed_capital pool) + treasury
    transition (deployed → pendingSweep). Sent by the round subsystem.
    The handler books TWO ledger legs: `refresh_forfeit` (retire the
    user claim) and `refresh_fee` (operator fee share). Both carry the
    same round_id; the partial unique index uses event_type to keep
    them distinct.
  - `SweepCompletedMsg` — Round-sweep reclamation into the treasury
    wallet. Sent by `batchsweeper`. Carries `MiningFeeSat` (absolute
    on-chain cost) so the handler books BOTH the `round_sweep` reclaim
    leg and a `mining_fees` expense leg. Also carries `ConsumedOutpoints`
    (sweep tx inputs, pre-inserted as `sweep_consumption`) and
    `ReturnOutpoints` (the single return-to-wallet output, pre-inserted
    as `sweep_return`) for UTXO diff attribution, preventing the
    classifier from double-booking sweep I/O as external_* events.
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
  writes are skipped but the in-memory snapshot tracker still
  advances so a later-wired audit store starts from the correct
  baseline. `WalletUTXOLogEntry.SourceID` is the optional 16-byte
  round_id / batch_id set by handler pre-inserts; nil for rows the
  diff loop produced itself.
- `UTXOAuditEvent`, `UTXOClassification` — Typed enums mirroring the
  `utxo_events` and `utxo_classifications` catalog tables seeded by
  migrations `000011_utxo_audit_log` and `000012_utxo_attribution`.
  Classification values: `deposit`, `withdrawal` (external created/spent),
  `sweep_return`, `sweep_consumption` (sweep tx created/spent),
  `round_funding` (round tx inputs from treasury), `round_change` (round
  tx outputs to treasury), `change` (legacy), `pending` (two-phase
  default: diff loop pre-writes pending, reconcile promotes to final),
  `unknown` (unclassified fallback).
- `utxoTracker` — In-memory snapshot of the treasury wallet UTXO set,
  accessed exclusively from the actor's single-consumer receive
  loop (no mutex). The first block after startup seeds the
  snapshot from the wallet; subsequent blocks diff against prev
  and emit audit rows for created / spent UTXOs. Ledger legs are
  NOT booked here -- see the audit-only invariant below.
  Rehydrated on startup by `reseedUTXOSnapshot` when a
  `UTXOSnapshotReader` is wired so a post-restart diff sees the
  right baseline and does not miss spends that happened during
  downtime.
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
    refresh round forfeits VTXOs. Producers must populate
    `TotalAmountSat` with the gross forfeited value and must supply
    `FundingOutpoints` / `ChangeOutpoints` for classifier attribution.
  - ← `batchsweeper` (via root wiring):
    `SweepCompletedMsg{ReclaimedAmountSat, MiningFeeSat, ConsumedOutpoints,
    ReturnOutpoints, ...}` on expired-VTXO sweep confirmation. Producers
    must populate `MiningFeeSat` with the absolute on-chain fee and
    supply the consumed/return outpoint slices for classifier attribution.
  - ← `oor`: `OORFinalizedMsg` after FinalizeAcceptedEvent (currently
    carries zero input/output amounts; handler skips the fee leg when
    `fee = input - output` is zero).
  - ← `chainsource` (self-registered on Start via
    `SubscribeBlocksRequest` + `MapBlockEpoch` adapter):
    `BlockEpochMsg` on each connected block. The ledger actor
    cancels its own subscription in `Stop` so the chain source does
    not keep telling a draining mailbox.

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
- **UTXO diff uses a two-phase classifier.** Round and sweep handlers
  pre-insert `wallet_utxo_log` rows with a `source_id` (round_id or
  batch_id) and the appropriate attributed classification
  (`round_funding`, `round_change`, `sweep_consumption`, `sweep_return`)
  before the next `BlockEpochMsg` arrives. The diff loop writes
  `classification='pending'` for movements it observes; a subsequent
  `PromotePendingWalletUTXOLog` reconcile pass promotes still-unattributed
  pending rows to `deposit` / `withdrawal` and books the corresponding
  `external_deposit` / `external_withdrawal` ledger legs. Round- and
  sweep-attributable movements already have their UNIQUE-constraint slot
  filled by the pre-insert, so the diff loop's ON CONFLICT DO NOTHING
  skips them and no double-counting occurs. The `UNIQUE(hash, index, event)`
  constraint plus the source_id index (on non-null rows) back this up.
- **UTXO diff replaces the snapshot only after writes succeed.** A
  `ListUnspent` error or an audit-row insert failure leaves the
  previous snapshot intact, so the next successful block retries
  naturally without duplicating audit rows (the UNIQUE(hash,
  index, event) constraint plus ON CONFLICT DO NOTHING at the db
  layer also backstops this).
- **External-fund idempotency keys are reserved.** `outpointKey`
  produces the 36-byte `outpoint_hash || little-endian-index`
  shape that `fees.RecordExternalDeposit` /
  `RecordExternalWithdrawal` expect, matching the client's
  `exitIdempotencyKey` layout. The helpers are currently unused
  from this package (see the audit-only invariant); the classifier
  PR wires them back in.
- **`WalletUTXOLister` and `UTXOAuditStore` are independently optional.**
  A deployment without a configured lister runs the actor as-is (no UTXO
  diff, just the message handlers). A deployment with a lister but
  no audit store still runs the diff loop and advances the in-
  memory tracker; only the persisted audit trail is disabled.
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
  post-restart baseline with `seeded=true`. Without rehydration, a
  post-restart diff would miss every spend that happened while the
  daemon was down (an empty prev snapshot plus post-downtime
  current set produces a created-only diff; disappeared outpoints
  never surface a spent audit row). An empty live set alone is NOT
  taken as evidence of a fresh install: reseed also checks
  `CountAuditRows`, and when rows exist but nothing is currently
  live (e.g. everything swept, pending boarding) the tracker flips
  to `seeded=true` with an empty snapshot so `seeded` remains an
  accurate liveness signal. The genuine seeding pass runs only
  when the audit log has zero rows ever written.
- **Start self-registers for block epochs.** When `ChainSource` is
  configured, `Start` issues a `SubscribeBlocksRequest` after the
  durable mailbox boots and installs a `MapBlockEpoch` adapter that
  turns every `chainsource.BlockEpoch` into a `BlockEpochMsg`
  delivered to the ledger actor's own mailbox. `Stop` cancels the
  subscription before draining the mailbox. Without this step, the
  UTXO diff subsystem's handler is unreachable from chain state and
  the audit log never advances.
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
