# daemonrpc

## Purpose

Daemon gRPC API definitions for wallet operations and round queries. Proto
source: `daemonrpc/daemon.proto`.

## Key Messages

- `TransactionHistoryEntry` — unified transaction history row. Fields include
  `output_index int32` (field 16, added in the current sweep): the transaction
  output index for the entry, or -1 when not applicable (e.g. plain boarding
  deposits). Populated from `chain_vout` on direct ledger rows or from the OOR
  binding outpoint index for virtual receive entries.

## Relationships

- **Depends on**: nothing (proto definitions).
- **Depended on by**: `darepod` (implements services), `cmd/darepocli` (uses generated clients).

## Invariants

- **Never edit generated code** — regenerate via `make rpc`.
