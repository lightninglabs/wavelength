# Canonical Activity Log

This document is the design-of-record for the canonical activity log
([#774](https://github.com/lightninglabs/darepo-client/issues/774)), the
foundational child of the wallet event-log epic
[#776](https://github.com/lightninglabs/darepo-client/issues/776). It defines the
storage schema, the stable-id contract, and the read/write model that the
sibling children (C2–C5) build on, so the wallet activity surface stops being
recomputed on every read.

The code it replaces lives in
[`swapwallet/history.go`](../swapwallet/history.go) (`listActivity`); the
durability pattern it borrows lives in the swapd↔client mailbox
(`swapdk-server/swapserver/mailbox_store.go`) and the double-entry
[`ledger`](../ledger).

## 1. Problem

The user-facing wallet **activity feed is recomputed on every read**. Each
`List`/`SubscribeWallet` call re-joins 4–6 live sources in `swapwallet/history.go`
(`listActivity`): the swap DB (`ListSwaps`), the double-entry ledger
(`ListTransactions`), boarding state (`GetBalance`), the VTXO inventory
(`ListVTXOs`), and process-local pending maps (`runtime.pendingSnapshot`). There
is no persisted, canonical record of an activity row.

Three structural consequences fall out of derive-on-read:

1. **No stable cross-restart id.** Only SEND-invoice and RECV rows carry a stable
   id (the Lightning `payment_hash`). EXIT, DEPOSIT, and on-chain sends surface a
   *pending* row under one id and a *confirmed* row under a different id, because
   no daemon-side hook links an exit's queued outpoints to its sweep txid, or a
   deposit's boarding address to its boarding txid (`swapwallet/doc.go`, "v1
   LIMITATIONS"). The flagship case is
   [#610](https://github.com/lightninglabs/darepo-client/issues/610): an on-chain
   send's row id *is the consumed VTXO outpoint* (`leaveEntryStub` keeps only
   `queuedOutpoints[0]`), which is destroyed seconds later when the round seals —
   the handle vanishes and cannot represent a multi-input sweep.
2. **No pending→settled reconciliation.** Append-only ledger rows have no
   reversing/terminal marker, so refunded sends and completed OORs can read
   PENDING indefinitely
   ([#613](https://github.com/lightninglabs/darepo-client/issues/613),
   [#569](https://github.com/lightninglabs/darepo-client/issues/569)). A
   cooperative-leave EXIT row reaches COMPLETE today only through a *read-time*
   forfeited-VTXO scan kept in a process-local map, which a restart loses
   ([#568](https://github.com/lightninglabs/darepo-client/issues/568),
   [#612](https://github.com/lightninglabs/darepo-client/issues/612)).
3. **A lossy, non-resumable subscription.** `Runtime.emit`
   (`swapwallet/runtime.go`) fans updates with a non-blocking send — slow
   consumers silently drop updates, possibly terminal ones. `SubscribeWallet`
   (`swapwallet/service.go`) has no cursor; `include_existing` re-snapshots from
   scratch on reconnect, so a client that disconnects cannot tell what it missed.

The lower layers already solve this class of problem, and they do it the same
way: with an **append-only, sequence-numbered log**. The double-entry `ledger` is
durable, append-only, and idempotent. The swapd↔client **mailbox**
(`swapdk-server/swapserver/mailbox_store.go`) is the reference design — and the
detail that matters most for C4 is *how* it sequences: `AppendMailboxEnvelope` is
a plain `INSERT … RETURNING event_seq`, so **every state change is a new,
immutable row with a new, higher `event_seq`**; `Pull(cursor)` returns
`event_seq > cursor` and never has to revisit a row. That append-only-per-event
shape is exactly what makes a reconnecting client's cursor sound. C1 brings the
same discipline up into the wallet-facing activity surface.

## 2. Goals / Non-goals

**Goals**

- A persisted store that is the single source of truth for `List` and
  `SubscribeWallet`, replacing the per-read multi-source merge.
- A **stable canonical id per operation**, allocated at request time and
  preserved from initiation through confirmation across daemon restarts, for
  *every* entry kind (SEND, RECV, DEPOSIT, EXIT — on-chain sends are EXIT-kind
  rows).
- An **append-only event sequence** that gives a reconnecting subscriber a
  gap-tolerant, no-missed-update cursor — the property C4 needs, supplied by C1's
  storage so C4 only has to define the wire protocol on top of it.
- A pagination cursor for `List(ACTIVITY)` that neither skips nor duplicates
  rows while the feed grows (issue
  [#781](https://github.com/lightninglabs/darepo-client/issues/781), A5).

**Non-goals (handled by sibling children, listed for boundaries)**

- C2 — the terminal/reversing/settlement *semantics* and the durable
  cooperative-leave jobs (this doc defines *where* those events are written and
  the daemon hooks they need, not their full state model).
- C3 — balance accounting. **Note:** the open balance bugs the epic groups here
  (#648, #760, #765) are *daemon-side missing-balance-bucket* gaps in
  `GetBalance`; they do not flow through `listActivity` and a client activity log
  does not fix them. C1 neither helps nor blocks them; they belong to a separate
  VTXO/boarding state→balance workstream.
- C4 — the resumable `SubscribeWallet` *wire protocol* (cursor request field,
  replay framing, the "you missed N" signal). C1 supplies the event stream and
  cursor semantics C4 reads; §3.4 states the requirement C1 must satisfy, not the
  protocol itself.
- C5 — startup reconciliation at the current chain tip.
- Asset-aware amounts (#632) and the mobile shape (#713) — kept in mind so the
  schema does not foreclose them, but out of scope here.

## 3. Design

### 3.1 Two tables: a current-state projection and an append-only event log

The read paths want two different things, so the design uses two tables rather
than overloading one. `List(ACTIVITY)` wants the *current state* of each
operation; `SubscribeWallet` wants the *stream of transitions* a client can
replay from a cursor. A single update-in-place table cannot serve both: if it
mutates a row in place and keeps the row's sequence number fixed, a reconnecting
subscriber whose cursor is already past that number never sees the row's later
transitions — the terminal PENDING→COMPLETE update is exactly what gets dropped.
That is the same silent-drop failure the current `Runtime.emit` has, so reusing a
single mutable-row table would not fix consequence (3). The mailbox avoids the
trap by appending one immutable row per event; C1 does the same.

Both tables ship as one additive sqlc migration
(`db/sqlc/migrations/000010_activity_log.{up,down}.sql`, regenerated via
`make sqlc`), following the existing enum-lookup-table convention.

The `kind` and `status` enums mirror the wire enums one-to-one —
`EntryKind` (`SEND`, `RECV`, `DEPOSIT`, `EXIT`) and `EntryStatus` (`PENDING`,
`COMPLETE`, `FAILED`) in `rpc/walletdkrpc/wallet.proto` — so a stored row maps to
a `WalletEntry` with no internal-to-wire translation and existing `kinds`/status
filters keep working. An on-chain send is an `EXIT`-kind row, as `normalize.go`
emits today; the canonical-id scheme (3.2) tells an on-chain send from a
unilateral exit by its leave-job record, not by a fifth kind. Confirmation
detail lives in `phase`/`confirmation_height` (mirroring `WalletEntry.progress`),
not in a separate `CONFIRMED` status.

```sql
-- Enum lookup tables (matching the ledger_event_types convention), seeded in
-- the same migration so the NOT NULL foreign keys are satisfiable. The values
-- are exactly the wire EntryKind / EntryStatus enums.
CREATE TABLE IF NOT EXISTS activity_kinds   (kind   TEXT PRIMARY KEY);
CREATE TABLE IF NOT EXISTS activity_statuses(status TEXT PRIMARY KEY);
INSERT INTO activity_kinds(kind) VALUES
    ('SEND'), ('RECV'), ('DEPOSIT'), ('EXIT')
    ON CONFLICT DO NOTHING;
INSERT INTO activity_statuses(status) VALUES
    ('PENDING'), ('COMPLETE'), ('FAILED')
    ON CONFLICT DO NOTHING;

-- activity_entries: the current-state projection, one row per operation,
-- keyed by its stable canonical id (3.2). List(ACTIVITY) reads this table.
-- The row is updated in place as the operation advances; its identity
-- (canonical_id) and its creation time never change.
CREATE TABLE IF NOT EXISTS activity_entries (
    canonical_id    TEXT PRIMARY KEY,

    kind            TEXT NOT NULL REFERENCES activity_kinds(kind),
    status          TEXT NOT NULL REFERENCES activity_statuses(status),

    -- Signed amount, matching the wire convention (WalletEntry.amount_sat is a
    -- signed int64); NOT the ledger_entries convention, which stores a positive
    -- magnitude plus a debit/credit direction. BIGINT, never INTEGER: on the
    -- Postgres backend a bare INTEGER is signed 32-bit and overflows above
    -- ~21.47 BTC. Every sat column in the existing schema is BIGINT.
    amount_sat      BIGINT NOT NULL,
    fee_sat         BIGINT NOT NULL DEFAULT 0,
    counterparty    TEXT NOT NULL DEFAULT '',
    note            TEXT NOT NULL DEFAULT '',

    -- Lifecycle projection the daemon already computes (A2/A3 fields).
    phase           TEXT NOT NULL DEFAULT '',
    failure_code    TEXT NOT NULL DEFAULT '',
    failure_reason  TEXT NOT NULL DEFAULT '',
    payment_hash    BLOB,
    txid            BLOB,
    confirmation_height INTEGER,
    vtxo_outpoint   TEXT NOT NULL DEFAULT '',
    request_json    TEXT NOT NULL DEFAULT '',  -- WalletEntryRequest oneof

    -- Correlation handles back to the source subsystems, typed to match the
    -- source columns (session_id/round_id/txid are BLOBs there) so the
    -- projector's update-by-correlator joins are same-type. Indexed below.
    swap_session_id BLOB,
    ledger_txid     BLOB,
    boarding_addr   TEXT,

    created_at_unix INTEGER NOT NULL,
    updated_at_unix INTEGER NOT NULL
);

-- The feed is read newest-first and paged by an IMMUTABLE key, so a row that
-- transitions in place keeps its position and pagination never skips or
-- duplicates. created_at never changes; canonical_id breaks ties.
CREATE INDEX IF NOT EXISTS activity_entries_created_idx
    ON activity_entries (created_at_unix DESC, canonical_id DESC);

CREATE INDEX IF NOT EXISTS activity_entries_swap_session_idx
    ON activity_entries (swap_session_id);

-- activity_events: the append-only event log, one immutable row per lifecycle
-- transition, mirroring the mailbox. SubscribeWallet reads this table.
CREATE TABLE IF NOT EXISTS activity_events (
    -- event_seq is monotonic and never reused. It is NOT contiguous: a failed
    -- or conflicting INSERT still burns a value, so consumers must treat "any
    -- event_seq > cursor is new" and never infer a gap means a dropped event.
    event_seq       INTEGER PRIMARY KEY AUTOINCREMENT,

    canonical_id    TEXT NOT NULL REFERENCES activity_entries(canonical_id),
    status          TEXT NOT NULL REFERENCES activity_statuses(status),
    phase           TEXT NOT NULL DEFAULT '',

    -- The projected WalletEntry as of this transition, so a replaying
    -- subscriber needs no second query and sees exactly what was emitted.
    entry_json      TEXT NOT NULL,

    created_at_unix INTEGER NOT NULL
);
```

A lifecycle transition does two writes in one transaction: it `UPDATE`s the
`activity_entries` row by `canonical_id` (advancing status/phase/txid/conf-height,
bumping `updated_at_unix`), and it `INSERT`s one `activity_events` row carrying
the new projection. `List` reads the first table; `SubscribeWallet` reads the
second.

### 3.2 Stable canonical id (the crux, fixes #610)

`canonical_id` is the operation's identity for its whole life. Today only two
kinds have a stable one; the other three are the work.

| Kind | canonical_id source | Status today |
|------|---------------------|--------------|
| SEND (invoice) / RECV | Lightning `payment_hash` | **Stable already.** `normalize.go` keys the swap row by `payment_hash`; the projection is a no-op. |
| OOR send / receive | OOR `session_id` | **Stable already.** Durably persisted (`oor_session_registry`, migration 000005) and RPC-exposed; adopting it is essentially a rename of the txid the row already uses. |
| SEND (on-chain) / EXIT | a durable leave-job id | **Does not exist yet — must be built.** |
| DEPOSIT | a durable deposit-address record id | **Does not exist yet — must be built.** |

The bottom two rows are the substance of #610, and the doc is explicit that they
are net-new daemon work rather than the reuse of an existing handle. The
identifiers that look reusable are not:

- The wallet-layer `send_intent_id` (`swapwallet/send_intents.go`) is an in-memory
  map entry with a 5-minute TTL that is consumed and deleted before dispatch. It
  never reaches the activity row, and a restart loses it.
- The daemon's `PendingIntentID` (`wallet/pending_intent.go`) is a hash of the
  *consumed forfeit outpoints* — the very handles #610 says are destroyed at round
  seal — is never returned in `SendOnChainResponse`, and is deleted when the round
  adopts the intent.

So today the on-chain-send/EXIT row falls back to `queuedOutpoints[0]`, which is
why #610 exists. C1 therefore must:

1. **Mint a stable id at request acceptance, daemon-side**, on a durable record
   that survives round adoption and restart (the leave-job record for
   EXIT/on-chain-send; the boarding-address record for DEPOSIT).
2. **Return that id** in the `LeaveVTXOs`/`SendOnChain` response (today it returns
   only `queued_outpoints` + status) and on the deposit-address response, so
   `swapwallet` can write it as `canonical_id`.
3. **Emit the eventual settlement handle** correlated to that id — the leave's
   commitment/sweep txid, the deposit's boarding txid — so the projector advances
   the *same* row to COMPLETE (recording the txid and confirmation height)
   instead of creating a second row.

Step 3 is the C2 reconciliation signal; #587 already delivered the daemon-side
pattern to mirror (a durable spending-reservation index plus a startup reconcile).
An acceptable alternative for the post-seal id is #610's own second option — use
the round commitment txid (observable forever) as the confirmed id, with the
request-time leave-job id as the pending handle and a correlation row linking the
two. Either way, the durable record and the RPC surface are the real cost, and
they are in scope for C1, not deferred.

### 3.3 Write path (projection)

A single projector — owned by `swapwallet`'s runtime — subscribes to the
lifecycle signals it already has (swap FSM transitions, ledger events,
boarding/VTXO state changes, the deadline overlay) and writes both tables forward
in one transaction per transition:

1. On request acceptance: `INSERT` the PENDING `activity_entries` row under the
   allocated `canonical_id`, and append the first `activity_events` row.
2. On each transition: `UPDATE` the `activity_entries` row by `canonical_id`
   (status/phase/txid/conf-height/failure_*), bump `updated_at_unix`, and append
   one `activity_events` row carrying the new projection — but **only when the
   projected state actually changed**. A redundant re-emit (the startup backfill,
   the swap monitor's include-existing replay) upserts the current-state row
   without appending a duplicate event, so the append-only log stays "one row per
   real transition" and a resumable subscriber never replays a bogus transition.
3. On terminal/reversing events (C2): set the terminal status deterministically
   (COMPLETE / FAILED / refunded-as-failed) — no more "stuck PENDING."

**Every producer must be a projector source.** The gap to watch is a producer
that reaches the user-facing feed by some path *other* than the runtime's emit
sites. The credit subsystem (sub-dust credit-only sends, #830) is exactly this
case: it is a separate actor whose rows are projected at *read time* in
`listActivity` (`collectCreditEntries`), not through `Runtime.emit`. If the read
path were cut over to the store without a credit-registry projector hook,
credit-only sends would vanish from the canonical log — the same class of bug
(#829) this epic exists to kill. So C1 hooks the credit registry too (mint
`canonical_id` = `payment_hash` at credit-op acceptance, advance it on terminal
state), and any future producer must be wired at its transition, not only at read
time.

**Write-path consistency: best-effort + startup reconcile (not outbox).** The two
activity tables are atomic *with each other* (one transaction), but the projection
is **not** committed in the same transaction as the underlying swap/ledger/credit
state write. Projection is best-effort: it runs project-then-emit and a store
error is logged, never blocking or failing the state change. A crash between the
state commit and the projection therefore defers an activity row, and the §3.5
startup backfill/reconcile is the load-bearing recovery that repairs it. The
alternative — a transactional outbox that appends the activity row in the *same*
txn as each producer's state write (mirroring the ledger's idempotent
`InsertClientLedgerEntry` against a partial unique index) — would close that crash
window at the cost of coupling every producer's commit to the activity schema.
This design deliberately chooses best-effort + reconcile for its simplicity; the
choice is stated explicitly here because C4's "no silent drops" guarantee leans on
whether the log is crash-atomic with state or eventually-consistent via reconcile,
and here it is the latter.

`List` and `SubscribeWallet` then read **only** from these tables. The multi-source
merge in `listActivity` is deleted once the projector is authoritative; the
existing collectors are reused once, during backfill (3.5).

### 3.4 Read path: pagination + the cursor C4 needs

- **`List(ACTIVITY)`** reads `activity_entries` ordered by `(created_at_unix DESC,
  canonical_id DESC)`, returning a `has_more` flag and an opaque cursor (the last
  `(created_at_unix, canonical_id)` pair). Because the ordering key never mutates,
  a row that transitions in place keeps its position: paging neither skips nor
  duplicates while the feed grows, which is A5/#781's acceptance criterion. This
  also removes the bounded-merge-window truncation derive-on-read suffers, since
  there is one table to page instead of N capped per-source pulls. `has_more` and
  the cursor are net-new fields on `ActivityList`/`ListRequest` (the daemon ledger
  already computes `has_more`; the ACTIVITY surface just never exposed it).
- **`SubscribeWallet`** (C4) reads `activity_events` from a monotonic `event_seq`
  cursor: on reconnect the daemon replays `event_seq > cursor` over immutable rows
  — the mailbox's `Pull`/`AckUpTo` pattern, faithful this time because the rows
  never change. The replay is gap-tolerant: a missing `event_seq` is a burned
  counter value, not a dropped event, so the consumer treats anything past its
  cursor as new. C1 supplies this stream and cursor; C4 defines the request field,
  the replay framing, and the "you fell behind, reconcile from N" signal.

### 3.5 Migration & rollout

- **Additive schema.** The sqlc migration only adds tables and seeds the enum
  rows; nothing existing changes. Down-migration drops them.
- **Backfill, with an honest limit.** On first start after the migration, run the
  existing derive collectors once to project current state into the store —
  including `collectCreditEntries` (#830), so credit-only sends are seeded
  alongside swaps, the ledger, boarding, and VTXO exits. This recovers full state
  only for the kinds that already have a stable id (SEND-invoice/RECV and credit
  via `payment_hash`, OOR via `session_id`). It **cannot**
  reconstruct a request-time `canonical_id` for in-flight EXIT/DEPOSIT/on-chain-send
  rows, because that id does not exist in durable state until 3.2's daemon records
  land — the pending handles live only in the in-memory `Runtime.pending` map.
  Backfill therefore projects confirmed ledger rows under their settlement id and
  starts minting stable ids for *new* operations; correlating older in-flight
  rows is C2 work. The projection is idempotent on `canonical_id`, so re-running
  is safe.
- **Dual-read transition.** Behind a flag, `List`/`Subscribe` read from the store
  while a test harness compares against the legacy merge. The comparison oracle
  must allow the *known-fixed* differences — the store deliberately diverges from
  the legacy merge exactly on the buggy rows (unified #610 ids, reconciled
  stuck-PENDING rows). The gate is "differs only in the known-fixed ways," not
  "matches the legacy merge." Because the proto has no deployed external consumers
  yet, a hard cutover is also cheap if dual-read proves more trouble than signal.
- **Proto stays additive.** The row already carries `request`/`progress` (#777)
  and `failure_code` (#779); `has_more`, the `List` cursor, and the `Subscribe`
  `event_seq` cursor are additive request/response fields.

### 3.6 Open questions

- **Where the daemon mints and persists the EXIT/DEPOSIT/on-chain-send id** — on
  the leave-job record vs. a dedicated intent record, and the exact response field
  that carries it up to `swapwallet`. 3.2 fixes that the id must be daemon-side and
  durable; the record's shape is open.
- **Ownership of reconciliation** (C2) — `swapwallet` vs. the swap subsystem; #587
  already delivered a daemon-side VTXO-reservation reconciliation pattern to mirror.
- **`request_json` / `entry_json` encoding** — protoJSON of the relevant message
  vs. flattened typed columns; JSON keeps the schema stable as request shapes
  evolve.
- **Coordination** with mobile (#713) and asset-aware amounts (#632) before the
  schema freezes.

The choice between append-only events and an update-in-place current state is not
an open question: §3.1 keeps both — a current-state projection for `List` and an
append-only event log for `Subscribe`.

## 4. Relationship to the event-log epic (#776)

C1 is the foundation the other children build on: **C2** writes
terminal/settlement events through the projector and supplies the daemon
correlation hooks 3.2 needs; **C4** reads `activity_events` for a resumable,
gap-tolerant subscribe; **C5** backfills/reconciles the store at the current tip
on startup. A5 pagination rides directly on §3.4. **C3 (balance) is deliberately
out of the line:** the open balance bugs are daemon-side missing-balance-bucket
gaps that bypass the activity feed, so they are tracked separately rather than as
consumers of this store.

## 5. References

- Code: `swapwallet/history.go` (`listActivity` derive-on-read),
  `swapwallet/normalize.go` (`leaveEntryStub`, the #610 outpoint id),
  `swapwallet/send_intents.go` (ephemeral `send_intent_id`),
  `wallet/pending_intent.go` (anchor-derived `PendingIntentID`),
  `swapwallet/doc.go` (v1 id limitations), `swapwallet/runtime.go` (`emit` lossy
  fan-out), `swapwallet/service.go` (`SubscribeWallet`),
  `swapdk-server/swapserver/mailbox_store.go` (`Append`/`Pull`/`AckUpTo`
  append-only reference), `db/sqlc/migrations` + `ledger`
  (append-only/enum-table/BIGINT precedent).
- Issues: epic #776; this child #774; resumable-subscribe child #775; id-stability
  symptoms #610, #612, #568, #569, #613; reconciliation prior art #587; pagination
  #781.
