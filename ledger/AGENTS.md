# ledger

## Purpose

Client-side durable actor that serializes all accounting writes (fees, VTXO
receipts/sends, exit costs) as double-entry ledger entries and UTXO audit log
records. Provides a crash-safe financial audit trail for tax reporting and fee
transparency.

## Chart of Accounts

The client ledger uses six accounts seeded by migration
`000006_fee_accounting.up.sql`:

- `wallet_balance` (asset) — on-chain wallet funds.
- `vtxo_balance` (asset) — current VTXO holdings.
- `fees_paid` (expense) — Ark protocol fees paid to the operator.
- `onchain_fees` (expense) — L1 chain/miner fees (exit costs, etc).
- `transfers_in` (revenue) — counterparty side of received VTXOs.
- `transfers_out` (expense) — counterparty side of sent VTXOs.

`transfers_in` and `transfers_out` are kept as separate accounts so gross
send and gross receive flows are visible independently instead of netted
on a single account. This matters for tax reporting where gross figures
are typically required.

## Key Types

- `LedgerActor` — Durable actor that processes accounting messages and persists ledger entries.
- `ActorConfig` — Configuration: logger, delivery store, ledger store, UTXO audit store, actor ID.
- `LedgerStore` — Interface for DB persistence of ledger entries (implemented by `db.LedgerStoreDB`).
- `LedgerEntry` — Domain-level double-entry record (debit/credit accounts, amount, round ID, event type).
- `UTXOAuditStore` — Interface for DB persistence of UTXO audit log entries (implemented by `db.UTXOAuditStoreDB`).
- `UTXOAuditEntry` — Domain-level UTXO audit record (outpoint, amount, event, block height, classification).
- `LedgerMsg` / `LedgerResp` — Message and response type constraints for the durable mailbox.
- `FeePaidMsg` — Records boarding/refresh fee payments.
- `VTXOReceivedMsg` — Records incoming VTXOs. `Source` must be one of `SourceRoundBoarding` (boarding/refresh of the client's own on-chain funds; offsets wallet_balance), `SourceRoundTransfer` (in-round receive from another participant; offsets transfers_in), or `SourceOOR` (out-of-round receive; offsets transfers_in). Any other value is rejected.
- `VTXOSentMsg` — Records outgoing VTXO transfers. Carries either `SessionID` (32-byte OOR) or `RoundID` (16-byte in-round) — exactly one must be non-zero; both-zero and both-set inputs are rejected.
- `ExitCostMsg` — Records a unilateral exit as two ledger entries: a send leg (`transfers_out` debit, `vtxo_balance` credit) for the net-of-fee value and a fee leg (`onchain_fees` debit, `vtxo_balance` credit) for the miner fee. Together the credits reduce `vtxo_balance` by the gross exited amount. Wallet-side movement is covered separately by the `wallet_utxo_log` audit trail.
- `UTXOCreatedMsg` — Records new wallet UTXO confirmations with classification.
- `UTXOSpentMsg` — Records wallet UTXO spends with classification.

## Relationships

- **Depends on**: `baselib/actor` (durable actor framework, TLV codec, service keys).
- **Depended on by**: `db` (provides `LedgerStoreDB` and `UTXOAuditStoreDB`), `darepod` (wires actor at startup).
- **Receives**:
  - `FeePaidMsg` — from round subsystem after fee confirmation.
  - `VTXOReceivedMsg` — from round/OOR subsystems on VTXO receipt.
  - `VTXOSentMsg` — from OOR subsystem on outbound transfer.
  - `ExitCostMsg` — from VTXO/chain subsystem on unilateral exit.
  - `UTXOCreatedMsg` — from wallet actor when a new UTXO is confirmed.
  - `UTXOSpentMsg` — from round/OOR/wallet subsystems when a UTXO is consumed.

## Invariants

- All accounting writes are serialized through a single durable actor instance (no concurrent DB writes).
- Every ledger entry is double-entry: debit and credit accounts must differ.
- Unknown fee types and VTXO sources return errors (no silent misclassification). Callers should use the exported `FeeType*`, `Source*`, and `Classification*` constants rather than literal strings so typos are caught at compile time.
- Zero-valued RoundIDs are stored as NULL via `roundIDOrNil` so the DB conditional unique index correctly bypasses idempotency checks for non-round events.
- Fire-and-forget pattern: `LedgerResp` is always nil; callers use `Tell`, not `Ask`.
- Messages use TLV stream encoding (variable-length fields) for forward-compatible extensibility.
- `Start` validates both `DeliveryStore` and `LedgerStore` are non-nil before launching the runtime.
- `UTXOAuditStore` is optional: when nil, UTXO audit messages are logged but not persisted.
- UTXO classification context is provided by the sending subsystem, not inferred from chain data.
- UTXO audit inserts are idempotent on `(outpoint_hash, outpoint_index, event)` via `ON CONFLICT DO NOTHING`, so RestartMessage replay after a crash is a silent no-op rather than a duplicate row.

## Deep Docs

- [docs/durable_actor_architecture.md](../docs/durable_actor_architecture.md) — CDC pattern, durable mailbox lifecycle.
- [docs/durable_actor_quickstart.md](../docs/durable_actor_quickstart.md) — TLVMessage, ActorBehavior, migration checklist.
- [db/CLAUDE.md](../db/CLAUDE.md) — DB layer including LedgerStoreDB and UTXOAuditStoreDB adapters.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
