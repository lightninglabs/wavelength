# batch

## Purpose

Batch transaction building and MuSig2 signing coordination for Ark rounds.
Constructs commitment transactions, VTXO trees, and orchestrates multi-party
nonce exchange and partial signature aggregation.

## Key Types

- `Terms` — Round parameters: sweep delay, max VTXOs per tree, fee rates, exit delays.
- `TxSignerCoordinator` — MuSig2 nonce exchange and partial signature aggregation.
- `TreeSignCoordinator` — Per-tree signing state management.
- `TreeContext` — Tree construction context (leaves, branches, tapscript info).

## Relationships

- **Depends on**: no internal dependencies (uses only external btcsuite/lnd packages).
- **Depended on by**: `rounds` (tx building and signing coordination).

## Invariants

- Nonce exchange must complete before partial signatures are requested.
- All tree leaves must be accounted for before signing begins.
- Aggregated signatures must be verified before finalization.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
