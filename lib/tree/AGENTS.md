# lib/tree

## Purpose

VTXO tree construction, materialization, and MuSig2 signing session management.
Builds the Merkle-like transaction tree structure used in Ark rounds, from leaf
descriptors through branch nodes to the batch output.

## Key Types

- `Tree` — Complete VTXO or connector tree: root outpoint, root output, node hierarchy, and traversal helpers. `Verify` checks structure plus value flow; `ValidateValueConservation` exposes the funding check separately for callers that have already bound `BatchOutput` to an authoritative prevout. Built via `BuildVTXOTree` or `BuildConnectorTree`.
- `Node` — Single tree node representing a transaction in the tree (branch or leaf).
- `LeafDescriptor` — Describes a single VTXO leaf: amount, owner pubkeys, cosigner keys, CSV delay, and `AssetAmount` (asset units in the leaf output; zero for Bitcoin-only trees).
- `VTXODescriptor` — Interface for VTXO metadata needed by tree construction (amount, cosigners, owner key).
- `ConnectorDescriptor` — Describes a connector output for forfeit transaction construction.
- `Structure` — Intermediate tree layout built by `BuildStructure` before materialization.
- `StructureConfig` — Configuration for tree building (radix, partition weight function).
- `SignerSession` — MuSig2 signing session for tree transactions, wrapping `input.MuSig2Signer`.
- `Materializer` / `BTCMaterializer` — Interface and implementation for materializing tree nodes into actual Bitcoin transactions. `Materializer.MaterializeNode` fills in a node's `Input`, `Outputs`, `FinalKey`, and `OutputsMeta` and returns the child `MaterializeParams`; `Materialize` walks a `Structure` through it. The interface is the seam that lets an asset tree reuse the BTC tree layout — `tapassets` implements it to commit one tap-sdk transition per node.
- `MaterializeParams` — Per-node materialization input; currently just `Input`, the outpoint the node spends.
- `AssetTreeContext` — Side-table of the extra state an asset tree needs, keyed by node input outpoint: per-node signing tweak, sealed transition package, leaf asset root, node asset amount, and the tree's asset ref. Populated by the materializer before the tree is shared; `Tree.AssetContext` is nil for Bitcoin-only trees.
- `BatchOutputSpec` / `BuildBatchOutputSpec` — The batch output plus its taproot material (untweaked MuSig2 aggregate `InternalKey`, operator `SweepLeaf`, `TapTreeBytes`). Callers use them to populate BIP-371 output metadata and to add an asset commitment. `BuildBatchOutput` remains for callers that need only the `*wire.TxOut`.
- `ComputeInternalKey` / `ComputeFinalKey` — Untweaked MuSig2 aggregate key, and the same key tweaked with a taproot root.
- `TreeAssembler` — Two-pass builder (`BuildStructure` then `Materialize`) driven by `TreeConfig`.
- `Queue[T]` — Generic queue used internally for BFS tree traversal.

## Relationships

- **Depends on**: `lib/arkscript` (taproot script construction, policy templates, `SpendInfo`).
- **Depended on by**: `round` (tree construction/validation), `oor` (tree references), `db` (tree serialization), `tapassets` (implements `Materializer` and populates `AssetTreeContext` for asset-carrying trees).

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
- **Cosigner slices are copied before MuSig2 aggregation.**
  `musig2.AggregateKeys` and a local signer's `MuSig2CreateSession` sort their
  input in place, so `ComputeInternalKey`, `ComputeFinalKey`, and
  `Node.NewSignerSession` all pass a `copyCosigners` copy. Aggregating
  `Node.CoSigners` directly would reorder a slice that callers and caches share,
  which is the cache-aliasing hazard below in its most silent form.
- **Asset state is carried beside the tree, not inside the node transactions.**
  `AssetTreeContext` is keyed by node input outpoint, and the signing session
  looks up a per-node taproot tweak rather than reusing
  `SweepTapscriptRoot` — an asset node's tweak commits to its own asset root.
  A missing tweak fails session construction instead of silently signing under
  the sweep root. `ExtractPathForCoSigners` / `ExtractPathForIndices` carry the
  same `*AssetTreeContext` pointer into the extracted tree, so it is subject to
  the cache-aliasing invariant below.
- **Cache-aliasing invariant**: a `*Tree` is effectively immutable once published from
  a builder or resolver. Multiple downstream consumers may share the same `*Tree`
  pointer through caches and ancestry-fragment slices. Silently mutating a shared
  tree's nodes or root would corrupt every aliasing reader. Callers that need to
  transform a tree must clone it first.

## Deep Docs

- [lib/CLAUDE.md](../CLAUDE.md) — Parent lib package overview.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
