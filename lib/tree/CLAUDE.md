# lib/tree

## Purpose

VTXO tree construction, materialization, and MuSig2 signing session management.
Builds the Merkle-like transaction tree structure used in Ark rounds, from leaf
descriptors through branch nodes to the batch output.

## Key Types

- `Tree` — Complete VTXO or connector tree: root outpoint, root output, node hierarchy, and traversal helpers. `Verify` checks structure plus value flow; `ValidateValueConservation` exposes the funding check separately for callers that have already bound `BatchOutput` to an authoritative prevout. Built via `BuildVTXOTree` or `BuildConnectorTree`.
- `Node` — Single tree node representing a transaction in the tree (branch or leaf).
- `LeafDescriptor` — Describes a single VTXO leaf: amount, owner pubkeys, cosigner keys, CSV delay, and `AssetAmount` (asset units carried by the leaf output; zero for ordinary Bitcoin-only trees).
- `VTXODescriptor` — Interface for VTXO metadata needed by tree construction (amount, cosigners, owner key).
- `ConnectorDescriptor` — Describes a connector output for forfeit transaction construction.
- `Structure` — Intermediate tree layout built by `BuildStructure` before materialization. Carries an `AssetContext` when the leaves declare asset amounts.
- `StructureConfig` — Configuration for tree building (radix, partition weight function).
- `AssetTreeContext` — Side-table of asset-tree state hung off `Tree.AssetContext` and `Structure.AssetContext`: per-node subtree asset amounts, per-node taproot signing tweaks, per-node sealed transfer packages, per-leaf asset commitment roots, and the tree's `AssetRef`. Nil for ordinary Bitcoin-only trees; `IsEmpty` distinguishes an allocated-but-unpopulated context. Populated by `tapassets` during materialization.
- `SignerSession` — MuSig2 signing session for tree transactions, wrapping `input.MuSig2Signer`.
- `ComputeInternalKey` / `ComputeFinalKey` — Untweaked MuSig2 cosigner aggregate, and the same aggregate with a taproot tweak applied. `ComputeInternalKey` exists for callers (notably `tapassets`) that must compose their own tweak instead of the sweep tapscript root.
- `Materializer` / `BTCMaterializer` — Interface and implementation for materializing tree nodes into actual Bitcoin transactions.
- `TreeAssembler` — Two-pass builder (`BuildStructure` then `Materialize`) driven by `TreeConfig`.
- `Queue[T]` — Generic queue used internally for BFS tree traversal.

## Relationships

- **Depends on**: `lib/arkscript` (taproot script construction, policy templates, `SpendInfo`).
- **Depended on by**: `round` (tree construction/validation), `oor` (tree references), `db` (tree serialization), `tapassets` (asset-aware materialization: builds the structure, drives `Materialize`, populates `AssetTreeContext`).

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
- **Cosigner slices are copied before aggregation and before signing.**
  `musig2.AggregateKeys` sorts its input in place, and a local
  `MuSig2Signer` may do the same, so `ComputeInternalKey`, `ComputeFinalKey`,
  and `Node.NewSignerSession` all pass a `copyCosigners` shallow copy.
  `Node.CoSigners` order is load-bearing elsewhere (it indexes the signing
  plan), so an in-place sort would silently reorder it under the caller.
- **Asset trees sign with per-node tweaks, ordinary trees with the sweep
  root.** `Tree.NewTreeSignerSession` consults `AssetContext.tweakLookup()`
  when `AssetContext` is non-nil and falls back to `SweepTapscriptRoot`
  otherwise; a node with no recorded tweak is a hard error rather than a
  silent fallback. Callers signing an asset tree must go through
  `Tree.NewTreeSignerSession`, not the package-level `NewSignerSession`.
- **Cache-aliasing invariant**: a `*Tree` is effectively immutable once published from
  a builder or resolver. Multiple downstream consumers may share the same `*Tree`
  pointer through caches and ancestry-fragment slices. Silently mutating a shared
  tree's nodes or root would corrupt every aliasing reader. Callers that need to
  transform a tree must clone it first.

## Deep Docs

- [lib/CLAUDE.md](../CLAUDE.md) — Parent lib package overview.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
