# lib/tx/oor

## Purpose

OOR-specific transaction builders and validators for out-of-round transfer
packages. Combines checkpoint and Ark transaction construction, validates the
structural relationships between them, and provides serialization for submit and
finalize packages.

## Key Types

- `SubmitPackage` — v0 OOR submit payload: Ark tx PSBT plus checkpoint PSBTs.
  SessionID derived from unsigned Ark txid.
- `FinalizePackage` — v0 OOR finalize payload: Ark tx PSBT plus finalized
  checkpoint PSBTs with tap tree metadata.
- `ValidatedSubmitPackage` — Output of structural validation ensuring canonical
  ordering, checkpoint/input matching, and witness UTXO presence.
- `CheckpointOutput` / `RecipientOutput` / `CheckpointInput` — Builder inputs
  for PSBT construction.
- `CheckpointArtifact` — Wraps checkpoint PSBT with tap tree sidecar metadata.

## Key Functions

- `BuildArkPSBT` — Constructs deterministic Ark PSBT spending checkpoint
  outputs, enforcing fee-less transfers and canonical ordering.
- `BuildCheckpointPSBT` — Wraps checkpoint.BuildPSBT with tap tree metadata.
- `ValidateSubmitPackage` / `ValidateFinalizePackage` — Structural validators
  ensuring canonical Ark PSBT, checkpoint set matches inputs, witness UTXOs
  present.
- `ApplyFinalizeData` — Applies finalize data to a validated submit package.

## Relationships

- **Depends on**: `lib/scripts`, `lib/tx/arktx` (validation, TxVersion),
  `lib/tx/checkpoint` (BuildPSBT), `lib/tx/psbtutil` (Serialize/Parse).
- **Depended on by**: `oor` (session state machine), `db` (artifact store),
  `rpc/oorpb`, `darepod` (RPC server).

## Invariants

- `SubmitPackage.SessionID` is stable (derived from unsigned Ark txid) across
  restarts and retries.
- Checkpoint set must exactly match Ark input references (no missing, no extra).
- Each Ark input spends checkpoint output index 0 (canonical v0 mapping).
- Witness UTXOs must be present in Ark PSBT inputs (package is self-contained).
- Tap tree metadata (`TapTreePSBTKey`) required on all Ark inputs for
  finalization.
- Fee-less constraint: sum(checkpoint inputs) == sum(recipient outputs excluding
  anchor).
- Anchor output always last with value 0.

## Deep Docs

- [ARCHITECTURE.md](../../../ARCHITECTURE.md) — System-wide package map.
