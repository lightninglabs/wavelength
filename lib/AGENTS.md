# lib

## Purpose

Shared domain utilities used across the codebase: tree construction, transaction
builders, Bitcoin script helpers, BIP-322 signing, cross-package actor message
interfaces, and core Ark types.

## Sub-Packages

### lib/tree
- `Tree` — Root node plus batch outpoint/output (encapsulates VTXO Merkle tree).
- `Node` — Individual tree node with children and outputs.
- `LeafDescriptor` — VTXO or connector output to include in tree construction.

### lib/tx
- `arktx` — Canonical output ordering for virtual-chain step transactions.
- `checkpoint` — Checkpoint transaction construction.
- `oor` — OOR-specific transaction builders.

### lib/types
- `OperatorTerms` — Server-published terms (key, delays, fee rate, dust limit).
- `JoinRoundRequest` — Primary round participation message.
- `VTXORequest`, `BoardingRequest`, `LeaveRequest`, `ForfeitRequest` — Sub-requests.

### lib/scripts
- Bitcoin script construction for Ark outputs (VTXO taproot, connectors, anchors).
- `AnchorPkScript` — Standardized P2A zero-value output for CPFP fee bumping.

### lib/bip322
- `Intent` — Application payload with ValidFrom/ValidUntil block height range.
- `Signer` — Interface for producing BIP-322 signatures over intents.

### lib/actormsg
- `VTXOActorMsg`, `RoundReceivable` — Marker interfaces avoiding import cycles.
- `VTXOActorServiceKey()`, `RoundActorServiceKey()` — Deterministic actor lookup.
- `TriggerBoardMsg` — Cross-package message from wallet→round for boarding (VTXO amounts).
- `TriggerVTXORefreshMsg`, `TriggerVTXOLeaveMsg` — Cross-package wallet→round triggers.

## Relationships

- **Depends on**: `baselib/actor` (actormsg only, for ServiceKey).
- **Depended on by**: `round`, `vtxo`, `oor`, `wallet`, `darepod`, `rpc/*`.

## Deep Docs

- [lib/bip322/README.md](bip322/README.md) — BIP-322 implementation guide.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
