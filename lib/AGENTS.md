# lib

## Purpose

Shared domain utilities used across the codebase: tree construction, transaction
builders, tapscript policy compilation, BIP-322 signing, cross-package actor
message interfaces, and core Ark types.

## Sub-Packages

### lib/tree
- `Tree` — Root node plus batch outpoint/output (encapsulates VTXO Merkle tree).
- `Node` — Individual tree node with children and outputs.
- `LeafDescriptor` — VTXO or connector output to include in tree construction.

### lib/arkscript
- `Node` — Sealed AST interface for tapscript spending conditions (Multisig, CSV, Condition, etc.).
- `VTXOPolicy` / `VHTLCPolicy` / `CheckpointPolicy` — High-level policy templates.
- `CompiledPolicy` — Fully compiled taproot tree with canonical leaf ordering.
- `PolicyTemplate` / `StandardVTXOParams` — Serializable policy template with helpers for encoding, decoding, and deriving pkScripts.
- `SpendInfo` / `AnchorPkScript` — Taproot spend helpers and standardized P2A anchor output construction.

### lib/tx
- `arktx` — Canonical output ordering and validation for Ark transactions.
- `checkpoint` — Checkpoint PSBT construction for OOR on-chain anchors.
- `oor` — OOR submit/finalize package builders and validators.
- `psbtutil` — PSBT encoding, decoding, and signature attachment helpers.

### lib/types
- `OperatorTerms` — Server-published terms (key, delays, fee rate, dust limit).
- `JoinRoundRequest` — Primary round participation message.
- `VTXORequest`, `BoardingRequest`, `LeaveRequest`, `ForfeitRequest` — Sub-requests.

### lib/bip322
- `Intent` — Application payload with ValidFrom/ValidUntil block height range.
- `Signer` — Interface for producing BIP-322 signatures over intents.

### lib/actormsg
- `VTXOActorMsg`, `VTXOManagerMsg`, `RoundReceivable` — Marker interfaces avoiding import cycles.
- `VTXOActorServiceKey()`, `VTXOManagerServiceKey()`, `RoundActorServiceKey()` — Deterministic actor lookup.
- `TriggerBoardMsg`, `RegisterIntentMsg` — Cross-package messages from wallet→round.
- `SelectAndReserveSpendRequest`, `ReserveForfeitRequest`, etc. — VTXO manager admission types.

## Relationships

- **Depends on**: `baselib/actor` (actormsg only, for ServiceKey).
- **Depended on by**: `round`, `vtxo`, `oor`, `wallet`, `darepod`, `rpc/*`.

## Deep Docs

- [lib/bip322/README.md](bip322/README.md) — BIP-322 implementation guide.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
