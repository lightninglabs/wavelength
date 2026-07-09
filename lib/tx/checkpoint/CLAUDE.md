# lib/tx/checkpoint

## Purpose

Helpers for constructing and validating Ark checkpoint transactions. Checkpoints
are taproot transactions that spend one or more VTXOs into a new on-chain output
with defined closure semantics, used as the on-chain anchor for out-of-round
transfers.

## Key Types

- `Input` — Describes a VTXO input being transformed into a checkpoint output
  (SpentVTXO identifier + OwnerLeafScript).
- `SpentVTXORef` — Groups spent VTXO outpoint and output data, ensuring callers
  don't mismatch identity and witness material.
- `Result` — Output of `BuildPSBT` containing the unsigned checkpoint PSBT.

## Key Functions

- `BuildPSBT` — Constructs an unsigned checkpoint PSBT spending a VTXO input,
  paying entire input value to a checkpoint P2TR output, appending a zero-value
  anchor.
- `EncodeTapTree` / `DecodeTapTree` — TLV-based tapscript leaf serialization
  compatible with waddrmgr format.

## Relationships

- **Depends on**: `lib/arkscript` (CheckpointPolicy, CheckpointTapScript),
  `lib/tx/arktx` (TxVersion, validation).
- **Depended on by**: `lib/tx/oor` (checkpoint PSBT construction for OOR
  transfers).

## Invariants

- Checkpoint output pkScript is derived deterministically from operator
  checkpoint policy + caller-provided owner leaf script.
- Minimum CSV delay is `MinCheckpointCSVDelay` (10 blocks).
- Checkpoint transactions must have exactly one anchor output placed last.
- Tap tree encoding uses TLV format matching waddrmgr for durability.

## Deep Docs

- [ARCHITECTURE.md](../../../ARCHITECTURE.md) — System-wide package map.
