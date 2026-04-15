# ledgeractor

## Purpose

Client-side durable actor that serializes all accounting writes (fees, VTXO
receipts/sends, exit costs) as double-entry ledger entries and UTXO audit log
records. Provides a crash-safe financial audit trail for tax reporting and fee
transparency.

## Key Types

- `LedgerActor` — Durable actor that processes accounting messages and persists ledger entries.
- `ActorConfig` — Configuration: logger, delivery store, ledger store, UTXO audit store, actor ID.
- `LedgerStore` — Interface for DB persistence of ledger entries (implemented by `db.LedgerStoreDB`).
- `LedgerEntry` — Domain-level double-entry record (debit/credit accounts, amount, round ID, event type).
- `UTXOAuditStore` — Interface for DB persistence of UTXO audit log entries (implemented by `db.UTXOAuditStoreDB`).
- `UTXOAuditEntry` — Domain-level UTXO audit record (outpoint, amount, event, block height, classification).
- `LedgerMsg` / `LedgerResp` — Message and response type constraints for the durable mailbox.
- `FeePaidMsg` — Records boarding/refresh fee payments.
- `VTXOReceivedMsg` — Records incoming VTXOs (round or OOR source).
- `VTXOSentMsg` — Records outgoing OOR transfers.
- `ExitCostMsg` — Records unilateral exit on-chain costs.
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
- Unknown fee types and VTXO sources return errors (no silent misclassification).
- Zero-valued RoundIDs are stored as NULL via `roundIDOrNil` so the DB conditional unique index correctly bypasses idempotency checks for non-round events.
- Fire-and-forget pattern: `LedgerResp` is always nil; callers use `Tell`, not `Ask`.
- Messages use TLV stream encoding (variable-length fields) for forward-compatible extensibility.
- `Start` validates both `DeliveryStore` and `LedgerStore` are non-nil before launching the runtime.
- `UTXOAuditStore` is optional: when nil, UTXO audit messages are logged but not persisted.
- UTXO classification context is provided by the sending subsystem, not inferred from chain data.

## Deep Docs

- [docs/durable_actor_architecture.md](../docs/durable_actor_architecture.md) — CDC pattern, durable mailbox lifecycle.
- [docs/durable_actor_quickstart.md](../docs/durable_actor_quickstart.md) — TLVMessage, ActorBehavior, migration checklist.
- [db/CLAUDE.md](../db/CLAUDE.md) — DB layer including LedgerStoreDB and UTXOAuditStoreDB adapters.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
