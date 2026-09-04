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
- `AssetTreeContext` — Side-table of per-tree asset state (subtree amounts,
  per-input signing tweaks, sealed transfer packages, leaf asset roots), kept
  out of `Node` so the Bitcoin tree stays asset-agnostic. `nil` on
  Bitcoin-only trees.
- `BatchOutputSpec` — Batch output plus the taproot material (`InternalKey`,
  `SweepLeaf`, BIP-371 tap tree) needed to commit an asset root to it.

### lib/arkscript
- `Node` — Sealed AST interface for tapscript spending conditions (Multisig, CSV, Condition, etc.).
- `VTXOPolicy` / `VHTLCPolicy` / `CheckpointPolicy` — High-level policy templates.
- `VHTLCTiming` / `VHTLCClaimWindow` — Shared vHTLC block-timing model with
  `ValidateOrder` (structural) and `ValidateClaimWindow` (chain-relative)
  admission checks; `DecodeVHTLCTiming` recovers the tuple from a template.
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
- `SelectedVTXO.ReserveEpoch` / `ReleaseSpendRequest.ReserveEpochs` — Monotonic
  reservation epoch echoed to a spend and presented back on release, so the
  manager can refuse a stale release whose reservation was superseded. Zero (or
  an absent map entry) releases unconditionally.

### lib/recovery
- `Proof` — Immutable unilateral-exit recovery graph for one target outpoint.
- `Session` / `SessionState` — Mutable planning state and its durable TLV
  projection, driven by broadcast/confirm/fail observations.

### lib/scripts
- Removed; superseded by `lib/arkscript`.

## Relationships

- **Depends on**: `baselib/actor` (actormsg only, for ServiceKey).
- **Depended on by**: nearly every client subsystem (`round`, `vtxo`, `oor`,
  `wallet`, `unroll`, `unrollplan`, `txconfirm`, `fraud`, `db`, `sdk`,
  `vhtlcrecovery`, `swapclientserver`, `waved`, `rpc`/`arkrpc`) — `lib`
  holds the shared domain types the rest of the client builds on.

## Deep Docs

- [lib/bip322/README.md](bip322/README.md) — BIP-322 implementation guide.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
