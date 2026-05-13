# batch

## Purpose

Batch transaction building and MuSig2 signing coordination for Ark rounds.
Constructs commitment transactions, VTXO trees, and orchestrates multi-party
nonce exchange and partial signature aggregation.

## Key Types

- `Terms` — Round parameters: sweep delay, max VTXOs per tree, fee rates, exit
  delays. `ConnectorDustAmount` must be > 0 (default 330 sats).
- `TxSignerCoordinator` — MuSig2 nonce exchange and partial signature
  aggregation. Supports persisting aggregated sigs on server-side VTXOTrees.
- `TreeSignCoordinator` — Per-tree signing state management.
- `TreeContext` — Tree construction context (leaves, branches, tapscript info).
- `VTXOSpendMetadata` — Self-contained per-VTXO spend metadata (outpoint, owner
  key, exit delay) persisted alongside tree data for downstream checkpoint
  construction.

## Relationships

- **Depends on**: no internal dependencies (uses only external btcsuite/lnd packages).
- **Depended on by**: `rounds` (tx building and signing coordination), `oor`
  (spend metadata for checkpoint construction).

## Invariants

- Nonce exchange must complete before partial signatures are requested.
- All tree leaves must be accounted for before signing begins.
- Aggregated signatures must be verified before finalization.
- Aggregated MuSig2 sigs are persisted on server VTXOTrees so they survive
  restarts and can be used for sweep transactions.
- **`TreeSignCoordinator.AddPartialSignatures` silently skips txids that the
  signer is not involved in** (using the precomputed `signerTxIndex`). This
  is intentional for multi-tree rounds: the operator fans out one merged nonce
  map across all `TreeSignCoordinator` instances; each client responds with a
  single merged signature map; each coordinator picks out its own txids and
  ignores the rest. An unknown txid is NOT an error; a txid the signer owns
  but fails to register IS an error.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
