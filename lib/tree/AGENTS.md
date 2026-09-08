# lib/tree

## Purpose

VTXO tree construction, materialization, and MuSig2 signing session management.
Builds the Merkle-like transaction tree structure used in Ark rounds, from leaf
descriptors through branch nodes to the batch output.

## Key Types

- `Tree` — Complete VTXO or connector tree: root outpoint, root output, node hierarchy, and traversal helpers. `Verify` checks structure plus value flow; `ValidateValueConservation` exposes the funding check separately for callers that have already bound `BatchOutput` to an authoritative prevout. Built via `BuildVTXOTree` or `BuildConnectorTree`.
- `Node` — Single tree node representing a transaction in the tree (branch or leaf).
- `LeafDescriptor` — Describes a single VTXO leaf: amount, owner pubkeys, cosigner keys, CSV delay. `AssetAmount` is the number of asset units in the leaf output; zero for a Bitcoin-only leaf.
- `AssetTreeContext` — Side table holding everything needed to materialize and sign an **asset** tree: the tree's `AssetRef`, a per-node signing tweak and asset amount, a per-leaf asset commitment root, and the per-leaf sealed transfer package. Reached via `Tree.AssetContext` (nil for Bitcoin-only trees) and populated by builders (`tapassets`) or by the proto decoder (`rpc/roundpb.TreeFromProto`) before the tree is shared.
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
- **Depended on by**: `round` (tree construction/validation), `oor` (tree references), `db` (tree serialization), `tapassets` (asset tree materialization; populates `AssetTreeContext`), `rpc/roundpb` (proto ⇄ tree conversion).

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
- **`AssetTreeContext` is an all-or-nothing description.** `Validate(root)`
  requires a non-empty asset reference, rejects the same asset input
  appearing under two nodes, and requires every node to be completely
  described. A nil context is valid and means "Bitcoin-only tree"; a
  partially-populated one is an error, never a best-effort. The context is
  what supplies the per-node signing tweak, so an incomplete context
  produces signatures against the wrong output key rather than a visible
  failure.
- The asset context is subject to the same cache-aliasing rule as the tree
  itself: populate it before the `*Tree` is published, and clone before
  transforming. `Tree.ExtractPathForCoSigners` / `ExtractPathForIndices`
  do exactly that — each returns a context cloned and re-scoped to the
  extracted root (`cloneForRoot`), so a client's single-leaf path keeps its
  own tweak and commitment root without aliasing the full tree's context.
- `Tree.NewTreeSignerSession` takes its per-node tweak from
  `AssetContext.tweakLookup()` when the context is present, and from
  `SweepTapscriptRoot` otherwise. The two are not interchangeable.
- **Cache-aliasing invariant**: a `*Tree` is effectively immutable once published from
  a builder or resolver. Multiple downstream consumers may share the same `*Tree`
  pointer through caches and ancestry-fragment slices. Silently mutating a shared
  tree's nodes or root would corrupt every aliasing reader. Callers that need to
  transform a tree must clone it first.

## Deep Docs

- [lib/CLAUDE.md](../CLAUDE.md) — Parent lib package overview.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
