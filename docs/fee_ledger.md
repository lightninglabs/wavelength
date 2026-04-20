# Fee Ledger and Double-Entry Accounting (Server)

This document is the authoritative reference for the
server-side fee ledger: what accounts exist, what every event
class writes, which subsystem emits each event, and how replay
safety is maintained. It complements the per-package docs:

- [fees/CLAUDE.md](../fees/CLAUDE.md) — `Record*` helpers,
  `LedgerStore` interface, `AccountID` / `LedgerEventType`
  types, and `TreasuryTracker`.
- [ledger/CLAUDE.md](../ledger/CLAUDE.md) — the durable actor
  and its UTXO diff subsystem.
- [db/CLAUDE.md](../db/CLAUDE.md) — the `LedgerStoreDB`
  adapter and the migration shape.
- [fee-model.md](fee-model.md) — the economic model that
  defines which fees get charged and why.

The client-side parallel lives in
[client/docs/fee_ledger.md](../client/docs/fee_ledger.md); the
two sides deliberately share layout conventions so a change on
one side has an obvious mirror on the other.

## Why double-entry

Single-number "running balance" bookkeeping cannot distinguish
between "funds moved between two places I already own" and
"funds arrived from outside the system." It also cannot
distinguish gross revenue (useful for tax reporting) from net
revenue (useful for P&L). Double-entry fixes both: every event
writes two rows (one debit leg, one credit leg) for the same
amount on two different accounts. Sum of all account balances
is always zero, and per-account sums give meaningful gross
figures per product line.

Two rules drive every decision in this package:

- **Every event produces exactly one debit + one credit, same
  amount, different accounts.** The DB enforces this via
  `CHECK (debit_account <> credit_account)` and
  `CHECK (amount_sat > 0)` on `ledger_entries`.
- **The account_type determines how a leg affects the
  balance.** Assets and expenses are debit-normal: debiting
  them increases the number, crediting them decreases. Revenue,
  liability, and equity are credit-normal: the mapping is
  inverted.

The mnemonic most bookkeepers use: **debit is left, credit is
right; assets and expenses grow to the left, everything else
grows to the right.**

## Chart of accounts

Nine accounts seeded by migration
`db/sqlc/migrations/000010_accounting.up.sql`:

| Account | Type | Meaning |
|---|---|---|
| `treasury_wallet` | asset | Operator's on-chain wallet funds. |
| `deployed_capital` | asset | Operator capital backing live VTXOs. |
| `user_vtxo_claims` | liability | Operator's obligation to VTXO holders. |
| `boarding_fee_revenue` | revenue | Fee revenue from boarding. |
| `refresh_fee_revenue` | revenue | Fee revenue from refreshes. |
| `offboard_fee_revenue` | revenue | Fee revenue from offboards. |
| `oor_fee_revenue` | revenue | Fee revenue from OOR transfers (zero today). |
| `mining_fees` | expense | L1 miner fee outflow. |
| `external_funding` | equity | Operator capital contributions / withdrawals. |

The four `*_fee_revenue` accounts stay split (rather than
collapsing into a single `operator_revenue`) so that
tax-reporting tooling can see gross per-product figures
directly from account balances. Collapsing them would require
parsing the `event_type` column after the fact — the schema
encodes the categorization instead.

`external_funding` is the equity counterparty for wallet UTXO
movements the ledger actor's UTXO diff subsystem cannot
attribute to a round or a sweep. Without it, unknown-origin
deposits into the treasury wallet would have no matching
credit leg and `treasury_wallet` would drift up without a
balancing source.

## Event types

The `ledger_event_types` enum seeds these values:

| Event | Summary |
|---|---|
| `boarding_deposit` | User's on-chain deposit enters the shared output (net of fee). |
| `boarding_fee` | Operator fee carved from a boarding input. |
| `refresh_forfeit` | User's old VTXO claim retired during a refresh. |
| `refresh_fee` | Operator fee collected during a refresh. |
| `refresh_new_vtxo` | New VTXO claim issued to the user after a refresh. |
| `offboard` | User's VTXO claim paid out on-chain via offboard. |
| `offboard_fee` | Operator fee collected during an offboard. |
| `mining_fee` | L1 miner fee paid for a round transaction. |
| `round_sweep` | Expired-VTXO sweep reclaims capital to the treasury wallet. |
| `capital_committed` | Operator capital moved from the wallet into deployed capital to fund a round. |
| `oor_transfer` | OOR transfer fee (zero today; plumbing for future). |
| `external_deposit` | Wallet UTXO diff detected an unattributable inflow (operator capital injection). |
| `external_withdrawal` | Wallet UTXO diff detected an unattributable outflow (operator capital extraction). |

The `event_type` column references this enum and, together
with `debit_account` and `credit_account`, forms the tuple that
the partial unique index keys on for replay dedup — see the
[Replay safety](#replay-safety) section.

## Message-to-leg routing

Producers Tell the ledger actor using the five TLV messages
declared in `ledger/messages.go`. Each handler dispatches to
one or more `fees.Record*` helpers. The (debit, credit, event)
triple for every helper lives in `fees/ledger.go` and is
tested end-to-end by `fees.TestRecordHelpersUseSeededAccounts`.

| Message | Handler step | Debit | Credit | Event |
|---|---|---|---|---|
| `RoundConfirmedMsg` | capital leg | `deployed_capital` | `treasury_wallet` | `capital_committed` |
| `RoundConfirmedMsg` | boarding fee | `deployed_capital` | `boarding_fee_revenue` | `boarding_fee` |
| `RoundConfirmedMsg` | mining fee | `mining_fees` | `treasury_wallet` | `mining_fee` |
| `VTXOsForfeitedMsg` | forfeit retire | `user_vtxo_claims` | `deployed_capital` | `refresh_forfeit` |
| `VTXOsForfeitedMsg` | refresh fee | `user_vtxo_claims` | `refresh_fee_revenue` | `refresh_fee` |
| `SweepCompletedMsg` | sweep reclaim | `treasury_wallet` | `deployed_capital` | `round_sweep` |
| `SweepCompletedMsg` | sweep mining fee | `mining_fees` | `treasury_wallet` | `mining_fee` |
| `OORFinalizedMsg` | OOR fee (if > 0) | `user_vtxo_claims` | `oor_fee_revenue` | `oor_transfer` |
| `BlockEpochMsg` | external deposit | `treasury_wallet` | `external_funding` | `external_deposit` |
| `BlockEpochMsg` | external withdrawal | `external_funding` | `treasury_wallet` | `external_withdrawal` |

Three fee-model events (`boarding_deposit`, `refresh_new_vtxo`,
`offboard`, `offboard_fee`) are not emitted by any producer on
this branch yet — their `Record*` helpers exist so future PRs
that add per-participant boarding / offboard accounting slot in
without schema or API changes.

Rejected payloads (negative amounts, unknown message types)
fail with `ErrInvalidMessage`; a caller typo or wire corruption
dead-letters instead of silently misclassifying the entry.

## Flow walkthroughs

Each section shows the exact ledger entries produced by a
given server-side flow and the per-account net effect.

### Round confirmation

A round's commitment transaction confirms on-chain. The ledger
actor receives one `RoundConfirmedMsg` carrying the aggregate
committed VTXO value, boarding-fee total, and mining-fee total
for the round.

```
emitter: rounds.Actor (on VTXOCreatedNotification, future wiring)

Event 1 (capital_committed):
  debit  deployed_capital   += committed
  credit treasury_wallet    += committed   (asset down)

Event 2 (boarding_fee, only when boarding_fee_sat > 0):
  debit  deployed_capital   += fee
  credit boarding_fee_revenue += fee       (revenue up)

Event 3 (mining_fee, only when mining_fee_sat > 0):
  debit  mining_fees        += fee         (expense up)
  credit treasury_wallet    += fee         (asset down)
```

Per-account net effect:
- `treasury_wallet` ↓ by (committed + mining_fee) — value
  leaves the on-chain wallet.
- `deployed_capital` ↑ by (committed + boarding_fee) — the
  boarding fee is "carved from the deposit before the claim is
  created," so it also lands in deployed capital before moving
  to revenue.
- `boarding_fee_revenue` ↑ by boarding_fee (gross boarding
  revenue for the period).
- `mining_fees` ↑ by mining_fee.

### Refresh forfeit

A refresh round forfeits a batch of VTXOs and collects a
refresh fee. The ledger actor receives one `VTXOsForfeitedMsg`
carrying the gross forfeited amount, the VTXO count, and the
operator fee share.

```
emitter: rounds.Actor (future wiring)

Event 1 (refresh_forfeit, only when total_amount_sat > 0):
  debit  user_vtxo_claims   += gross   (liability down)
  credit deployed_capital   += gross   (asset up)

Event 2 (refresh_fee, only when refresh_fee_sat > 0):
  debit  user_vtxo_claims     += fee     (liability down)
  credit refresh_fee_revenue  += fee     (revenue up)
```

Per-account net effect:
- `user_vtxo_claims` ↓ by (gross + fee) — the user's
  outstanding claim retires on refresh.
- `deployed_capital` ↑ by gross — the old claim's backing
  capital returns to the deployed-capital pool, ready to be
  swept out to the wallet when the expired-VTXO sweep lands.
- `refresh_fee_revenue` ↑ by fee.

Both legs share `round_id`; the partial unique index uses
`event_type` to keep them distinct under replay. The handler
then calls `TreasuryTracker.OnVTXOsForfeited` to move the
forfeited capital from `deployed` to `pendingSweep` in memory
so congestion pricing stays smooth across the forfeit-to-sweep
window -- the ledger does not yet carry a separate pending
bucket (both states live in `deployed_capital`), so the split
is reconstructed only in memory.

### Sweep completion

An expired-VTXO sweep confirms on-chain and the operator's
capital cycles back from deployed to the wallet. The sweep
transaction itself costs on-chain mining fees, which the
ledger books as a separate expense leg so `treasury_wallet`
stays reconciled with the wallet's actual on-chain balance.

```
emitter: batchsweeper (future wiring)

Event 1 (round_sweep, only when reclaimed_amount_sat > 0):
  debit  treasury_wallet   += reclaimed   (asset up)
  credit deployed_capital  += reclaimed   (asset down)

Event 2 (mining_fee, only when mining_fee_sat > 0):
  debit  mining_fees       += fee         (expense up)
  credit treasury_wallet   += fee         (asset down)
```

Per-account net effect:
- `treasury_wallet` ↑ by (reclaimed − mining_fee) — net wallet
  inflow after paying the miner.
- `deployed_capital` ↓ by reclaimed.
- `mining_fees` ↑ by mining_fee.

Without the mining-fee leg, `treasury_wallet` would drift
behind on-chain reality by the cumulative sweep-tx cost every
cycle. Both legs share `batch_id` as the idempotency key;
`event_type` differentiates them in the partial unique index.
`TreasuryTracker.OnSweepCompleted` drains the matching
reclaim amount from the `pendingSweep` bucket that
`OnVTXOsForfeited` had inflated.

### OOR transfer

OOR transfers are free today, so the handler logs the finalize
event and skips the ledger write. When OOR fees are
introduced the handler will take the positive input-minus-
output delta and call `RecordOORTransfer`:

```
emitter (future): oor.TransferCoordinatorActor on
                  FinalizeAcceptedEvent

Event 1 (oor_transfer, only when fee > 0):
  debit  user_vtxo_claims   += fee
  credit oor_fee_revenue    += fee
```

The plumbing exists today so the future activation is a
single-site handler change, not a schema/API change.

### Wallet UTXO diff (external movements)

Each `BlockEpochMsg` triggers the ledger actor's UTXO diff
subsystem (when `WalletUTXOLister` is configured). The
subsystem lists the operator's treasury wallet UTXOs, compares
them against its in-memory snapshot, writes `wallet_utxo_log`
audit rows for every created/spent outpoint, and — after the
first "seeding" pass — books ledger entries for unclassified
movements.

```
emitter: chainsource / batchwatcher (future wiring)

Created UTXO classified as deposit:
  debit  treasury_wallet   += amount       (asset up)
  credit external_funding  += amount       (equity up)

Spent UTXO classified as unknown:
  debit  external_funding  += amount       (equity down)
  credit treasury_wallet   += amount       (asset down)
```

Both legs are keyed on a 36-byte
`outpoint_hash || little-endian-index` idempotency key, so a
redelivered `BlockEpochMsg` or a recomputed diff over the same
block is a silent no-op against the partial unique index.

The first block after startup runs a _seeding pass_ that
writes audit rows but skips the ledger legs: UTXOs that
predate the actor's view have prior origin elsewhere (they
were already accounted for on a previous operator deployment
or bootstrapped from migration seed), and double-counting them
as new external capital contributions would permanently skew
the equity account.

Classification today is intentionally naive ("every unknown
created is a deposit, every unknown spent is an unknown /
withdrawal"). Round-funding / sweep-return / change
attribution lives behind the `UTXOClassification` enum and
waits on round and sweep tracking state that is not yet
available on the server side — the classifier is the single
point to update when that state lands.

## Emission sites

The ledger actor is fully implemented but its producers are
plumbed in follow-up PRs. For this branch the actor accepts
messages and writes ledger / audit rows; the Tell sites land
with the next slice of the project.

| Subsystem | File (planned) | Messages emitted |
|---|---|---|
| `rounds` | `rounds/actor.go` | `RoundConfirmedMsg`, `VTXOsForfeitedMsg` |
| `batchsweeper` | root wiring | `SweepCompletedMsg` |
| `oor` | `oor/actor.go` | `OORFinalizedMsg` |
| `chainsource` / `batchwatcher` | root wiring | `BlockEpochMsg` |

Producers hold an `fn.Option[ledger.Sink]` (mirrored from the
client's `ledger.Sink` pattern) so Tell emission is a
one-liner on the hot path and a no-op when the ledger is not
wired.

## Startup & Recovery

The ledger actor carries two pieces of in-memory state that
must be reconstructed on restart or they silently drift from
the persisted ledger: the `TreasuryTracker`'s capital buckets
(which drive congestion pricing) and the `utxoTracker`'s UTXO
snapshot (which drives external-deposit attribution). Before
the durable mailbox accepts any message, `LedgerActor.Start`
runs two rehydration passes.

### TreasuryTracker rehydration (`reseedTreasuryTracker`)

When both a `TreasuryTracker` and a `LedgerBalanceReader` are
configured, `Start`:

1. Calls `reader.GetAccountBalance(deployed_capital)` and
   `reader.GetAccountBalance(treasury_wallet)` via the
   `ListLedgerEntries`-backed `GetAccountBalance` sqlc query
   (single-pass conditional aggregation: debits add, credits
   subtract).
2. Calls `TreasuryTracker.Reseed(deployedCapitalSat,
   pendingSweepSat=0, liveVTXOCount=0, walletBalance)` with
   the totals.

The tracker becomes a projection of the persisted ledger on
every startup. Two known approximations:

- **pendingSweep folds into deployedCapital.** The ledger's
  `deployed_capital` account carries both "backing live
  VTXOs" and "forfeited, awaiting sweep" at the same time;
  the in-memory split was lost on restart, so the projection
  conservatively treats everything as deployed. Subsequent
  `OnVTXOsForfeited` / `OnSweepCompleted` events re-establish
  the split as traffic flows. Over-counting pending as
  deployed inflates utilization, which biases congestion
  pricing upward rather than silently suppressing it.
- **liveVTXOCount resets to zero.** The schema tracks
  satoshi amounts, not VTXO counts; the count catches up as
  new `RoundConfirmedMsg` events arrive post-restart.

Without this pass, every restart leaves `deployedCapital` at
zero and `Utilization()` reads zero until enough round events
slowly rebuild the counter -- congestion pricing silently
suppresses during the warm-up window.

### UTXO snapshot rehydration (`reseedUTXOSnapshot`)

When both a `WalletUTXOLister` and a `UTXOSnapshotReader` are
configured, `Start`:

1. Calls `reader.ListLiveWalletUTXOs(ctx)`, which runs the
   `ListLiveWalletUTXOs` sqlc query against
   `wallet_utxo_log`: every `event='created'` row without a
   paired `event='spent'` row is live.
2. If the result is empty (fresh install), leaves the
   tracker unseeded so the first block epoch still performs
   the genuine baseline pass.
3. Otherwise, loads the UTXOs into `utxoTracker.prev` and
   flips `seeded=true`.

The first post-restart block epoch now attributes new UTXOs
as real `external_deposit` / `external_withdrawal` events
instead of silently folding them into a fresh seeding pass.
This closes the "treasury deposit during downtime is
silently lost" bug: any operator top-up that confirmed while
the daemon was down surfaces correctly on the first block
after restart.

### Recovery invariants

- Both rehydration passes are no-ops when their upstream
  dependency is absent (missing tracker, missing reader,
  missing lister). Unit-test harnesses that wire only a
  subset still Start cleanly.
- A reader error short-circuits `Start` so the actor never
  opens its mailbox with half-populated in-memory state.
- Rehydration runs BEFORE `PrependRestartMessage`, so the
  runtime's own restart hook observes already-correct
  tracker and snapshot state.

## Idempotency and replay safety

Durable-actor delivery is at-least-once. Any ledger write must
be safe to replay. The schema enforces this via one partial
unique index on `ledger_entries`:

- `uniq_ledger_idempotency` on
  `(idempotency_key, event_type, debit_account, credit_account)
  WHERE idempotency_key IS NOT NULL`.

The `InsertLedgerEntry` query uses `ON CONFLICT DO NOTHING`,
so a replayed insert matching an existing row on this tuple is
a silent no-op.

The `Record*` helpers stamp `IdempotencyKey` for the caller:

- Round-scoped events derive the key from the 16-byte
  `round_id`. Different events in the same round (e.g.
  `capital_committed` + `boarding_fee` + `mining_fee`) share
  the same key but differ on `event_type` / debit / credit, so
  the unique tuple does not collide.
- OOR-scoped events derive the key from the 32-byte
  `session_id`.
- External wallet events derive the key from the 36-byte
  `outpoint_hash || LE index` of the moved UTXO.

The `wallet_utxo_log` table uses a parallel scheme:
`UNIQUE(outpoint_hash, outpoint_index, event)` +
`ON CONFLICT DO NOTHING`, so re-running the per-block diff
after a crash is a silent no-op rather than a duplicate-row
error. `(hash, index)` alone cannot be unique because an
outpoint legitimately appears twice over its lifetime (once
`created`, once `spent`).

## Round / session mutual exclusion

Each ledger entry belongs to at most one of "round" or "OOR
session" — never both. The schema enforces this via
`CHECK (round_id IS NULL OR session_id IS NULL)`. External
wallet events (deposits, withdrawals) leave both nil and route
correlation through the `idempotency_key` column instead.

This shape is what keeps the three-way dedup story sane:
round-keyed, session-keyed, and outpoint-keyed entries are
each disambiguated on the same index tuple, just with
different `idempotency_key` shapes.

## Clock injection

`LedgerActor.clk` is a `clock.Clock` resolved at construction
from `ActorConfig.Clock` (falling back to
`clock.NewDefaultClock()`). Handlers stamp `CreatedAt` via
`a.clk.Now()` rather than reading `time.Now()` directly so
tests can inject a deterministic clock and assert on
timestamps without wall-clock races. The UTXO diff subsystem
shares the same clock source, so per-block event ordering is
stable across test runs.

## Deferred items

- **Producer wiring.** `rounds`, `batchsweeper`, `oor`, and
  the chainsource / `batchwatcher` block loop do not yet Tell
  messages into the ledger actor. The plumbing seam
  (`fn.Option[ledger.Sink]` on producer configs, mirrored from
  the client) is the follow-up PR's scope.
- **Classification for UTXO diff.** Currently everything
  created is "deposit" and everything spent is "unknown". A
  follow-up PR will track recent round-funding and sweep-return
  outpoints so the classifier can attribute them correctly
  before reaching `external_funding`.
- **Reconciliation job.** A periodic cross-check that the
  ledger-computed `treasury_wallet` balance matches the
  wallet's live balance (modulo in-flight events) needs its
  own PR. It depends on the round / sweep classifier landing
  first.
- **GetFeeHistory / EstimateFee RPCs.** Server-side RPC
  surfaces (paralleling the client's RPC layer in
  darepo-client#222) are deferred.

## Related documents

- [fees/CLAUDE.md](../fees/CLAUDE.md) — Record helper
  contracts, `LedgerStore` interface, typed `AccountID` /
  `LedgerEventType`.
- [ledger/CLAUDE.md](../ledger/CLAUDE.md) — durable actor,
  UTXO diff, replay invariants.
- [db/CLAUDE.md](../db/CLAUDE.md) — schema and partial
  unique index details.
- [fee-model.md](fee-model.md) — economic model, fee
  formulas, per-event accounting table.
- [client/docs/fee_ledger.md](../client/docs/fee_ledger.md)
  — client-side mirror of this doc.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — system-wide
  package map.
