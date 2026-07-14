# lib/tx

## Purpose

Transaction construction utilities for Ark protocol transactions: forfeit
transactions, VTXO collaborative signing, and related helpers. Sub-packages
handle specific transaction types (Ark batch, checkpoint, OOR, PSBT utilities).

## Key Types

- `BuildForfeitTx(vtxoOutpoint, vtxoAmount, connectorOutpoint, connectorAmount, serverForfeitScript)` — Constructs a forfeit transaction spending a VTXO via the collaborative tapscript path to a connector output. The penalty output value is `vtxoAmount + connectorAmount` so that zero-fee connector dust is swept correctly.
- `BuildForfeitTxWithContext(vtxoOutpoint, vtxoAmount, connectorOutpoint, connectorAmount, serverForfeitScript, ctx)` — Policy-aware forfeit construction using `ForfeitTxContext` to supply tapscript leaves and witness material derived from `lib/arkscript`.
- `ForfeitTxContext` — Per-VTXO context carrying the owner-leaf policy script and spend witness data needed for policy-backed forfeit transactions.
- `ValidateForfeitTx` — Validates a forfeit transaction structure and amounts.
- `VTXOSpendContext` — Contextual data for spending a VTXO (outpoint, amount, tapscript, internal key).
- `ConnectorSpendContext` — Contextual data for the connector input in a forfeit transaction.
- `NewVTXOCollabSignDescriptor` — Creates a sign descriptor for VTXO collaborative (MuSig2) signing.
- `NewForfeitPrevOutFetcher` — Constructs a `PrevOutputFetcher` for forfeit transaction signing.
- `ForfeitTxParams` — Parameters for forfeit transaction validation.

## Sub-Packages

- `lib/tx/arktx` — Ark batch transaction construction (commitment tx building, input/output assembly).
- `lib/tx/checkpoint` — OOR checkpoint transaction construction (2-of-2 collab path).
- `lib/tx/oor` — OOR-specific transaction helpers (transfer package assembly).
- `lib/tx/psbtutil` — PSBT utility functions (serialization, merging, input/output helpers).

## Relationships

- **Depends on**: `lib/arkscript` (taproot script construction, policy types).
- **Depended on by**: `round` (forfeit construction/validation), `oor` (checkpoint/Ark signing),
  `vtxo` (forfeit signing), `waved` (forfeit signature broker, RPC server).

## Invariants

- `ForfeitVTXOInputIndex` is 0; `ForfeitConnectorInputIndex` is 1. Forfeit transactions always have the VTXO as the first input and the connector as the second.
- Forfeit transaction construction is deterministic given the same inputs.

## Deep Docs

- [lib/CLAUDE.md](../CLAUDE.md) — Parent lib package overview.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
