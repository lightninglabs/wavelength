# lib/scripts

## Purpose

Low-level Bitcoin script construction helpers for standard Ark outputs (VTXO and
checkpoint tapscripts). Provides deterministic script builders and witness
construction for taproot spending paths.

## Key Types

- `VTXOLeafType` — Enum identifying VTXO tapscript tree leaves:
  `VTXOCollabPathLeaf` (index 0) and `VTXOTimeoutPathLeaf` (index 1).
- `VTXOSpendData` — Witness script + control block needed to spend a VTXO via
  collab or timeout path.

## Key Functions

- `VTXOTapScript` / `VTXOTapKey` — Full VTXO tapscript and taproot output key
  construction.
- `NewVTXOSpendInfo` / `VTXOSignDesc` — Derive spend info and sign descriptors
  for any leaf path.
- `VTXOTimeoutSpendWitness` / `VTXOCollabSpendWitness` — Construct witnesses
  for the two VTXO spending paths.
- `CheckpointOORScript` — Checkpoint script helpers for OOR transactions.
- `AnchorPkScript` — Standardized P2A zero-value output for CPFP fee bumping.

## Relationships

- **Depends on**: (no internal repo imports; pure script library).
- **Depended on by**: `darepod`, `db`, `lib/tx/*`, `round`, `vtxo`, `wallet`,
  `walletcore`.

## Invariants

- VTXO tree structure is immutable: NUMS key path (unspendable), collab leaf
  (index 0, owner CHECKSIGVERIFY + cosigner CHECKSIG), timeout leaf (index 1,
  owner CHECKSIG + CSV delay + DROP).
- Collab spend requires both owner and cosigner signatures. Timeout spend
  requires owner signature only (after CSV delay).
- Witness stack ordering: collab = [cosigner_sig, owner_sig, script,
  control_block]; timeout = [owner_sig, script, control_block].
- ARK NUMS key is unspendable (no known discrete log); key path is never
  usable.

## Deep Docs

- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
