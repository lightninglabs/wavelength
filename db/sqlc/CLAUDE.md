# db/sqlc

## Purpose

Generated type-safe SQL query layer for darepo-client's main persistent
store. Produced by `make sqlc` from query files in `db/sqlc/queries/` and
schema files in `db/sqlc/schemas/`. Supports both SQLite and PostgreSQL.
Never edit generated `*.go` files directly.

## Key Generated Types

- `Querier` interface — Top-level generated interface with all query methods
  grouped by domain.
- **Boarding**: `BoardingAddress`, `BoardingIntent`, `BoardingAddrRow`,
  `BoardingIntentRow`, `BoardingSweep`, `BoardingSweepInput`,
  `NewAddrParams`, `NewIntentParams`, `InsertBoardingSweepParams`.
- **Rounds**: `Round`, `RoundRow`, `UpdateRoundStatusParams`.
- **VTXOs**: `Vtxo`, `VtxoRow`, `VtxoAncestryPath`.
- **OOR Artifacts**: `OorPackage`, `OorVtxo`, `OwnedReceiveScript`.
- **Fee Accounting**: `LedgerEntry`, `Account`,
  `InsertClientLedgerEntryParams`.
- **Unilateral Exit**: `UnilateralExitJob`, `UpsertUnilateralExitJobParams`.
- **vHTLC Recovery**: `VhtlcRecoveryJob`.
- **Spending Reservations**: `SpendingReservation`,
  `UpsertSpendingReservationParams`.
- **Internal Keys**: `InternalKey`, `InsertInternalKeyParams` — shared
  registry for client wallet keys referenced by FK from boarding, VTXO,
  OOR, and round tables.
- **Chain Info**: `ChainInfo`.

## Migration History (17 versions)

| Version | Table(s) Added |
|---------|----------------|
| 000001 | `chain_info` |
| 000002 | `boarding_addresses`, `boarding_intents`, `boarding_statuses` |
| 000003 | `rounds`, `round_statuses` |
| 000004 | `oor_packages`, `oor_vtxos`, `owned_receive_scripts` + enum tables |
| 000005 | `chain_depth` column on `vtxos` |
| 000006 | `accounts`, `account_types`, `ledger_entries` |
| 000007 | `wallet_utxo_log` |
| 000008 | `unilateral_exit_jobs` |
| 000009 | `vtxo_ancestry_paths` (replaces scalar tree_path) |
| 000010 | `tx_proof` BLOB on `boarding_intents` |
| 000011 | `boarding_sweeps`, `boarding_sweep_inputs` |
| 000012 | `boarding_sweep_fee_paid` ledger event type |
| 000013 | `pending_board_requests` |
| 000014 | `chain_txid`/`chain_vout`/`confirmation_height` on `ledger_entries` |
| 000015 | `vhtlc_recovery_jobs` |
| 000016 | `exit_policy_kind`/`exit_policy_ref` on `unilateral_exit_jobs` |
| 000017 | `spending_reservations` |

`LatestMigrationVersion = 17`.

## Relationships

- **Depends on**: SQLite (`modernc.org/sqlite`) and PostgreSQL (`lib/pq`)
  drivers; standard library only.
- **Depended on by**: `db` (wraps generated queries in domain stores),
  `db/actordelivery/sqlc` (separate migration table, same pattern).

## Invariants

- **Never edit generated code** — regenerate via `make sqlc`.
- Query files live in `db/sqlc/queries/`; schema in `db/sqlc/schemas/`;
  migrations in `db/sqlc/migrations/`.
- `spending_reservations` uses `(outpoint_hash, outpoint_index)` as PK;
  a row's existence signals a durably checkpointed spend owner.
- `ledger_entries.entry_id` uses `AUTOINCREMENT` (SQLite) / `SERIAL`
  (PostgreSQL) to prevent rowid reuse after deletion.

## Deep Docs

- [db/CLAUDE.md](../CLAUDE.md) — Parent db package overview with domain
  store types and migration notes.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
