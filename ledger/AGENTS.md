# ledger

## Purpose

Client-side accounting actor. It serializes live-process accounting writes for
fees, VTXO receives/sends, wallet UTXO deposits, and unilateral exit costs
into SQL double-entry ledger rows plus `wallet_utxo_log` audit rows.

This package no longer uses the durable actor or durable mailbox framework.
The actor is an in-memory concurrency boundary. Restart safety lives in SQL:
idempotent ledger inserts, idempotent UTXO audit inserts, persisted producer
state, and replayable higher-level workflows.

Ledger event types include `wallet_utxo_created`, `boarding_fee_paid`,
`refresh_fee_paid`, `onchain_fee_paid`, `boarding_sweep_fee_paid`,
`vtxo_received`, and `vtxo_sent`.

## Chart of Accounts

The client ledger uses accounts seeded by
`db/sqlc/migrations/000006_fee_accounting.up.sql`:

- `wallet_balance` (asset) - on-chain wallet funds.
- `vtxo_balance` (asset) - current VTXO holdings.
- `fees_paid` (expense) - Ark protocol fees paid to the operator.
- `onchain_fees` (expense) - L1 chain/miner fees.
- `transfers_in` (revenue) - counterparty side of received VTXOs.
- `transfers_out` (expense) - counterparty side of sent VTXOs.
- `opening_balance` (equity) - source of funds for wallet UTXO deposits.

`transfers_in` and `transfers_out` stay separate so gross receive and send
flows remain visible instead of being netted.

See [docs/fee_ledger.md](../docs/fee_ledger.md) for the flow walkthrough.

## Key Types

- `LedgerActor` - in-memory actor that serializes accounting handlers and
  persists ledger/audit entries. It caches a resolved `clock.Clock`.
- `ActorConfig` - logger, `LedgerStore`, optional `UTXOAuditStore`, actor ID,
  and clock. There is no `DeliveryStore`.
- `Sink` - `actor.TellOnlyRef[LedgerMsg]` used by producers that want a
  fire-and-forget accounting sink.
- `LedgerStore` - DB persistence surface implemented by `db.LedgerStoreDB`.
- `LedgerEntry` - double-entry ledger record with optional
  `IdempotencyKey` for outpoint-keyed dedup.
- `UTXOAuditStore` / `UTXOAuditEntry` - DB persistence surface and domain row
  for wallet UTXO audit events.
- `LedgerMsg` / `LedgerResp` - in-memory message surface. Messages embed
  `actor.BaseMessage`; they are not serialized to a durable actor mailbox.
- `FeePaidMsg` - records boarding, refresh, and boarding-sweep fee payments.
- `VTXOReceivedMsg` - records incoming VTXOs. `Source` must be one of
  `SourceRoundBoarding`, `SourceRoundRefresh`, `SourceRoundTransfer`, or
  `SourceOOR`.
- `VTXOSentMsg` - records outgoing VTXOs. Exactly one of `SessionID` or
  `RoundID` must be set. `Outpoint` can disambiguate multi-VTXO round events
  through an outpoint-derived idempotency key.
- `ExitCostMsg` - records unilateral exit cost as send plus miner-fee ledger
  legs.
- `UTXOCreatedMsg` - writes a wallet UTXO audit row and a
  `wallet_utxo_created` ledger row.
- `UTXOSpentMsg` - writes wallet UTXO spend audit rows.

## Relationships

- **Depends on**: `baselib/actor` as an in-memory actor runtime and
  `lnd/clock` for injected time.
- **Depended on by**: `db`, `darepod`, `round`, `oor`, `vtxo`, and wallet
  flows.
- **Receives**:
  - From `round`: boarding/refresh/transfer VTXO events and refresh fees.
  - From `oor`: OOR send and receive materialization events.
  - From `vtxo`: unilateral exit cost events.
  - From wallet observation: wallet UTXO created/spent audit events.

## Caller Contract

Handlers book entries exactly as written. There is no hidden reconciliation.
Callers must emit the right pair or sequence of messages:

- Wallet UTXO confirmed: `UTXOCreatedMsg` writes both the audit row and the
  `debit wallet_balance, credit opening_balance` ledger row.
- Boarding: `VTXOReceivedMsg{Source=SourceRoundBoarding}` books
  `debit vtxo_balance, credit wallet_balance`.
- Refresh or directed-send self-change: emit both `VTXOSentMsg{Outpoint,
  RoundID, AmountSat=gross}` and
  `VTXOReceivedMsg{Source=SourceRoundRefresh, RoundID, AmountSat=gross}`.
  The pair cancels on `transfers_out`; the fee message carries the net loss.
- In-round transfer receive: `VTXOReceivedMsg{Source=SourceRoundTransfer}`.
- OOR receive: `VTXOReceivedMsg{Source=SourceOOR}`.
- OOR send: `VTXOSentMsg` with `SessionID` set.
- In-round send: `VTXOSentMsg` with `RoundID` set.
- Unilateral exit: `ExitCostMsg` expands into a send leg plus an on-chain
  fee leg.

## Durability Model

- The actor gives ordering and single-threaded reasoning inside one process.
  It is not itself the durability boundary.
- Ledger inserts use DB uniqueness on round ID, session ID, or explicit
  idempotency key. Redelivery from a replayed producer is a no-op.
- UTXO audit inserts are idempotent on `(outpoint_hash, outpoint_index,
  event)`.
- Multi-leg handlers call store methods under the caller/context transaction
  when one is present via `actor.TxFromContext`.
- If a producer needs crash-retry for an accounting-producing step, that retry
  must come from the producer's persisted SQL state/effect row, not from a
  ledger mailbox.

## Invariants

- All live accounting writes serialize through one actor instance.
- Every ledger entry is double-entry: debit and credit accounts must differ.
- Non-positive amounts, unknown fee types, unknown VTXO sources, and
  ambiguous IDs return `ErrInvalidMessage`.
- Zero-valued round/session IDs are stored as NULL so partial unique indexes
  apply only to events that actually have those IDs.
- `LedgerResp` is always nil; callers use `Tell`.
- `UTXOAuditStore` is optional. When nil, audit messages are logged but not
  persisted.
- UTXO classification context is supplied by the sender, not inferred from
  chain data.
- Handler errors log at `WarnS`.

## Deep Docs

- [docs/fee_ledger.md](../docs/fee_ledger.md) - client flow accounting.
- [db/CLAUDE.md](../db/CLAUDE.md) - SQL stores and generated query layer.
- [ARCHITECTURE.md](../ARCHITECTURE.md) - system-wide package map.
