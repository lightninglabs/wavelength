# ledger

## Purpose

Client-side durable actor that serializes all accounting writes (fees,
VTXO receipts/sends, wallet UTXO deposits, exit costs) as double-entry
ledger entries and UTXO audit log records. Provides a crash-safe
financial audit trail for tax reporting and fee transparency.

Event types: `wallet_utxo_created`, `wallet_utxo_spent`,
`wallet_sweep_transfer`, `boarding_fee_paid`, `refresh_fee_paid`,
`onchain_fee_paid`, `boarding_sweep_fee_paid`, `vtxo_received`,
`vtxo_sent`.

## Chart of Accounts

Seeded by `000006_accounting.up.sql`:

- `wallet_balance` (asset) — on-chain wallet funds.
- `vtxo_balance` (asset) — current VTXO holdings.
- `fees_paid` (expense) — Ark protocol fees paid to the operator.
- `onchain_fees` (expense) — L1 chain/miner fees (exit costs, etc).
- `transfers_in` (revenue) — counterparty side of received VTXOs.
- `transfers_out` (expense) — counterparty side of sent VTXOs.
- `opening_balance` (equity) — source-of-funds for every confirmed
  wallet UTXO. Without this account, `wallet_balance` would drift
  negative on every boarding.
- `wallet_clearing` (asset) — temporary clearing account for
  wallet sweep inputs, returns, chain cost, and external transfers.

`transfers_in` / `transfers_out` are separate accounts so gross send and
gross receive flows stay visible independently (tax reporting needs
gross figures, not nets).

For per-flow walkthroughs see
[docs/fee_ledger.md](../docs/fee_ledger.md).

## Key Types

For field-level detail, use `go doc github.com/lightninglabs/wavelength/ledger.<Symbol>`.

- `LedgerActor` — durable actor processing accounting messages. Runs on the
  durable Read/Commit (`TxBehavior`) path: each handler books its ledger legs
  inside one short, lease-fenced Commit transaction rather than holding a
  writer tx across the whole `Receive`. The `bindStores` factory injects a
  `ledgerTx` (typed store pair) bound to each Commit transaction. Caches the
  resolved `clock.Clock` at construction so handlers stamp `CreatedAt` without
  re-optioning the field.
- `ActorConfig` — logger, delivery store, ledger store, UTXO audit
  store, actor ID, optional `Clock` (`fn.Option[clock.Clock]`); None
  falls back to `clock.NewDefaultClock()`.
- `Sink` — alias for `actor.TellOnlyRef[LedgerMsg]`, via
  `NewSink(system)`. Producers (round / OOR / VTXO / wallet) hold
  `fn.Option[ledger.Sink]` and fire-and-forget.
- `LedgerStore` — single `InsertLedgerEntry` method. Multi-leg
  handlers rely on the durable actor's outer tx for atomicity, not a
  batch API. Implemented by `db.LedgerStoreDB`.
- `LedgerEntry` — double-entry record (debit/credit accounts, amount,
  round ID, session ID, event type, description, created_at, optional
  `IdempotencyKey []byte` for outpoint-keyed dedup, and structured
  `ChainTxid`/`ChainVout` columns stamped on chain-anchored events).
- `exitIdempotencyKey(hash, index)` / `walletUTXOIdempotencyKey` —
  derive 36-byte `outpoint_hash || outpoint_index` dedup keys
  distinct from round-keyed and session-keyed entries (separate
  `idx_client_ledger_idempotent_key` partial unique index).
- `UTXOAuditStore` / `UTXOAuditEntry` — UTXO audit log persistence
  (outpoint, amount, event, block height, classification). Implemented
  by `db.UTXOAuditStoreDB`.
- `LedgerMsg` / `LedgerResp` — mailbox constraint types.
- `FeePaidMsg` — boarding/refresh fee payments.
- `VTXOReceivedMsg` — incoming VTXOs. `Source` must be one of
  `SourceRoundBoarding` (own wallet → VTXO; offsets wallet_balance),
  `SourceRoundRefresh` (refresh / directed-send self-change; offsets
  transfers_out so the paired `VTXOSent` cancels on that account and
  only the operator fee moves vtxo_balance), `SourceRoundTransfer`
  (in-round receive; offsets transfers_in), or `SourceOOR` (OOR
  receive; offsets transfers_in). Any other value is rejected.
  `handleVTXOReceived` stamps the structured `ChainTxid` (32-byte
  outpoint hash) and `ChainVout` columns on the row in addition to
  the dedup key, so downstream consumers (notably `ListTransactions`
  → walletdkrpc onchain view) surface the commitment outpoint without
  text-parsing the description.
- `VTXOSentMsg` — outgoing VTXO. Carries either `SessionID` (32-byte
  OOR) or `RoundID` (16-byte in-round) — exactly one must be
  non-zero; both-zero and both-set are rejected. Optional `Outpoint`
  disambiguates per-VTXO for in-round multi-VTXO events (e.g. paired
  refresh emissions). Optional `IdempotencyKey` disambiguates
  round-scoped recipient/leave outflows that do not have a local VTXO
  outpoint.
- `ExitCostMsg` — unilateral exit as two ledger entries: send leg
  (`transfers_out` ⇐⇒ `vtxo_balance` net-of-fee) + fee leg
  (`onchain_fees` ⇐⇒ `vtxo_balance` miner fee). Wallet-side movement
  is covered separately by the `wallet_utxo_log` audit trail.
- `UTXOCreatedMsg` — wallet UTXO confirmations with classification.
  `handleUTXOCreated` writes TWO rows: `wallet_utxo_log` audit row
  AND a ledger row keyed by an outpoint-derived idempotency key.
  Deposit-like classifications credit `opening_balance`; boarding
  sweep returns credit `wallet_clearing`.
- `UTXOSpentMsg` — wallet UTXO spends. Audit-only for every
  classification except `ClassificationBoardingSweepInput`, which also
  books `debit wallet_clearing, credit wallet_balance`. The boarding
  sweep producer no longer sends per-leg messages (see
  `BoardingSweepConfirmedMsg`); the classification branch is retained
  for direct callers and the non-positive guard is scoped to it so an
  audit-only zero-amount spend cannot poison the mailbox.
- `BoardingSweepConfirmedMsg` — the consolidated confirmed-sweep event.
  Carries the sweep txid, chain cost (miner fee + P2A anchor), the list
  of spent inputs, and the destination (`DestinationExternal` plus
  amount). `handleBoardingSweepConfirmed` expands it into every clearing
  leg — fee, per-input audit + `wallet_clearing` debit, and the
  destination leg (external `transfers_out` settlement or wallet-return
  deposit) — inside ONE Commit, so `wallet_clearing` either nets to zero
  or nothing is written. Replaces the earlier fan-out of
  `FeePaidMsg{FeeTypeOnchainSweep}` + `UTXOSpentMsg` +
  `UTXOCreatedMsg`/`WalletSweepTransferMsg`, which could strand value in
  `wallet_clearing` on a partial emission.

## Relationships

- **Depends on**: `baselib/actor` (durable actor framework, TLV codec,
  service keys), `lnd/clock` (injectable time source).
- **Depended on by**: `db` (provides `LedgerStoreDB`,
  `UTXOAuditStoreDB`), `waved` (wires actor; exposes
  `LedgerStoreDB` to RPC), `round` / `oor` / `vtxo` / `wallet` (hold
  `fn.Option[ledger.Sink]` and Tell on hot-path transitions).
- **Receives** (via `Sink` Tell):
  - ← `round` (origin-routed on `VTXOCreatedNotification`):
    `VTXOReceivedMsg{SourceRoundBoarding}`, paired `VTXOSentMsg` +
    `VTXOReceivedMsg{SourceRoundRefresh}`,
    `VTXOReceivedMsg{SourceRoundTransfer}`; explicit recipient/leave
    `VTXOSentMsg` outflows; one `FeePaidMsg` per positive
    `OperatorFeeSat`, typed as boarding or refresh by the round
    composition.
  - ← `oor`: `VTXOSentMsg` after `FinalizeAcceptedEvent`;
    `VTXOReceivedMsg{SourceOOR}` per descriptor in
    `notifyMaterializedVTXOs`.
  - ← `unroll`: `ExitCostMsg` after the final sweep confirms, with
    gross value from the proof target output and fee derived from the
    persisted sweep transaction.
  - ← `wallet`: `UTXOCreatedMsg` on confirmed wallet UTXO observation
    plus one `BoardingSweepConfirmedMsg` per confirmed boarding sweep
    (the single atomic event the ledger expands into all clearing legs).

## Caller Contract

Handlers book ledger entries exactly as written — no hidden netting
or balance reconciliation. Required emission pairs:

| Flow | Required emission |
|------|-------------------|
| Wallet UTXO confirmed (deposit) | `UTXOCreatedMsg` (handler writes audit + ledger rows). |
| Boarding (wallet → VTXO) | `VTXOReceivedMsg{SourceRoundBoarding}` plus `FeePaidMsg{FeeTypeBoarding}` when `OperatorFeeSat > 0`. |
| Refresh / directed-send self-change | Paired `VTXOSentMsg{Outpoint,RoundID,gross}` + `VTXOReceivedMsg{SourceRoundRefresh,RoundID,gross}`; real vtxo_balance change comes from the round-emitted `FeePaidMsg{FeeTypeRefresh}` when `OperatorFeeSat > 0`. |
| In-round participant receive | `VTXOReceivedMsg{SourceRoundTransfer}` net. No `FeePaidMsg`. |
| OOR receive | `VTXOReceivedMsg{SourceOOR}` net. No `FeePaidMsg`. |
| OOR send | `VTXOSentMsg{SessionID}` net. No `FeePaidMsg`. |
| In-round send | `VTXOSentMsg{RoundID}` net. Recipient/leave sends without outpoints must set `IdempotencyKey`. `SessionID`/`RoundID` are mutually exclusive. |
| Unilateral exit | `ExitCostMsg{AmountSat=gross, ExitCostSat=fee}`. Handler expands to send-leg + fee-leg internally. |

## Invariants

- All accounting writes serialize through one durable actor instance
  (no concurrent DB writes).
- Every ledger entry is double-entry: debit and credit must differ.
- Handlers reject non-positive `AmountSat` with `ErrInvalidMessage`
  so a malformed TLV dead-letters cleanly instead of hitting
  `CHECK (amount_sat > 0)` and driving infinite nack-retry.
- Unknown fee types and VTXO sources error out (no silent
  misclassification). Use the exported `FeeType*`/`Source*`/
  `Classification*` constants, not string literals.
- Zero-valued RoundIDs/SessionIDs are stored as NULL via
  `roundIDOrNil` / `sessionIDOrNil` so the DB partial unique indexes
  correctly bypass idempotency checks for non-round/non-session
  events.
- Fire-and-forget: `LedgerResp` is always nil; callers `Tell`, never
  `Ask`.
- TLV stream encoding. `decodeAmountSat` narrows `uint64` to `int64`
  (rejects values past `MaxInt64`); `decodeFixedBytes` enforces exact
  lengths (`RoundID=16`, `SessionID=32`, `OutpointHash=32`).
- `Start` validates both `DeliveryStore` and `LedgerStore` are
  non-nil.
- `UTXOAuditStore` is optional: nil ⇒ audit messages log but don't
  persist.
- UTXO classification context comes from the sending subsystem, not
  from chain data.
- UTXO audit inserts are idempotent on
  `(outpoint_hash, outpoint_index, event)` via
  `ON CONFLICT DO NOTHING`.
- **Replay safety**: `InsertClientLedgerEntry` uses
  `ON CONFLICT DO NOTHING` against every partial unique index
  (`idx_client_ledger_idempotent_round`, `_session`, `_key`). Crash
  atomicity for multi-leg events (ExitCost) comes from the durable
  actor's outer tx: handlers run inside
  `TxAwareDeliveryStore.ExecTx`; `db.TransactionExecutor.ExecTx`
  joins via `actor.TxFromContext` so two `InsertLedgerEntry` calls
  commit atomically with the mailbox ack.
- Handler-level errors (`ErrInvalidMessage` + DB failures) log at
  `WarnS`. Error-level is reserved for internal bugs; both handler
  failure classes are externally triggered.

## Deep Docs

- [docs/fee_ledger.md](../docs/fee_ledger.md) — Per-flow account
  movements.
- [docs/durable_actor_architecture.md](../docs/durable_actor_architecture.md)
  — CDC pattern, durable mailbox lifecycle.
- [docs/durable_actor_quickstart.md](../docs/durable_actor_quickstart.md)
  — TLVMessage, ActorBehavior, migration checklist.
- [db/CLAUDE.md](../db/CLAUDE.md) — `LedgerStoreDB` and
  `UTXOAuditStoreDB` adapters.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
