# lib/tree

## Purpose

VTXO tree construction, materialization, and MuSig2 signing session management.
Builds the Merkle-like transaction tree structure used in Ark rounds, from leaf
descriptors through branch nodes to the batch output.

## Key Types

- `Tree` — Complete VTXO or connector tree: root outpoint, root output, node hierarchy, and traversal helpers. `Verify` checks structure plus value flow; `ValidateValueConservation` exposes the funding check separately for callers that have already bound `BatchOutput` to an authoritative prevout. Built via `BuildVTXOTree` or `BuildConnectorTree`.
- `Node` — Single tree node representing a transaction in the tree (branch or leaf).
- `LeafDescriptor` — Describes a single VTXO leaf: amount, owner pubkeys, cosigner keys, CSV delay.
- `VTXODescriptor` — Interface for VTXO metadata needed by tree construction (amount, cosigners, owner key).
- `ConnectorDescriptor` — Describes a connector output for forfeit transaction construction.
- `Structure` — Intermediate tree layout built by `BuildStructure` before materialization.
- `StructureConfig` — Configuration for tree building (radix, partition weight function).
- `SignerSession` — MuSig2 signing session for tree transactions, wrapping `input.MuSig2Signer`.
- `Materializer` / `BTCMaterializer` — Interface and implementation for materializing tree nodes into actual Bitcoin transactions.
- `TreeAssembler` — Two-pass builder (`BuildStructure` then `Materialize`) driven by `TreeConfig`.
- `Queue[T]` — Generic queue used internally for BFS tree traversal.

## Relationships

- **Depends on**: `lib/arkscript` (taproot script construction, policy templates, `SpendInfo`).
- **Depended on by**: `round` (tree construction/validation), `oor` (tree references), `db` (tree serialization).

## Invariants

- `DefaultRadix` is 2 (binary tree). Each internal node has at most 2 children.
- `NumLeafOutputs` is 2 per leaf transaction (VTXO output + sweep output).
- Cosigner keys must be deduplicated (`UniqueCosigners`) before computing the final MuSig2 key.
- Tree materialization is deterministic given the same leaf descriptors and operator key.
- `ValidateVTXODescriptors` / `ValidateConnectorDescriptor` must pass before tree construction.
- `Tree.Verify` requires a non-nil `BatchOutput`, checks that every reachable
  node spends its declared parent output, enforces monetary bounds, and
  requires each node to preserve the parent's exact value. Tree transactions
  pay zero fee for v3 ephemeral-anchor relay. Callers accepting an untrusted
  tree must bind `BatchOutput` to the authoritative commitment output before
  calling `Verify` or `ValidateValueConservation`. Extracted client paths retain
  every node output but prune unrelated child nodes, so value validation sums
  all outputs and recurses only into retained children. The traversal rejects
  cycles and nodes shared by multiple parents. `Node.Verify` checks only
  parent-child outpoint topology and is not a trust-boundary validator.
- **Cache-aliasing invariant**: a `*Tree` is effectively immutable once published from
  a builder or resolver. Multiple downstream consumers may share the same `*Tree`
  pointer through caches and ancestry-fragment slices. Silently mutating a shared
  tree's nodes or root would corrupt every aliasing reader. Callers that need to
  transform a tree must clone it first.

## Deep Docs

- [lib/CLAUDE.md](../CLAUDE.md) — Parent lib package overview.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
