# lib/tree

## Purpose

VTXO tree construction, materialization, and MuSig2 signing session management.
Builds the Merkle-like transaction tree structure used in Ark rounds, from leaf
descriptors through branch nodes to the batch output.

## Key Types

- `Tree` — Complete VTXO or connector tree: root outpoint, root output, node hierarchy, and traversal helpers. `Verify` checks structure plus value flow; `ValidateValueConservation` exposes the funding check separately for callers that have already bound `BatchOutput` to an authoritative prevout. Built via `BuildVTXOTree` or `BuildConnectorTree`.
- `Node` — Single tree node representing a transaction in the tree (branch or leaf).
- `LeafDescriptor` — Describes a single VTXO leaf: amount, owner pubkeys, cosigner keys, CSV delay, and `AssetAmount` (the asset units in the leaf output; zero for a plain Bitcoin tree).
- `AssetTreeContext` / `NewAssetTreeContext` — Sidecar carrying everything an *asset* tree needs beyond its Bitcoin shape: per-node subtree asset amounts, per-node taproot signing tweaks, per-leaf asset commitment roots, per-node sealed transfer packages, and the tree's asset reference. `Tree.AssetContext` is nil for a plain Bitcoin tree; `Structure.AssetContext` is populated by `BuildStructure` when any leaf carries a non-zero `AssetAmount`. `Validate(root)` checks the context completely describes the tree.
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
- **The asset context is keyed by node *and* by input outpoint.** A structure
  pass runs before materialization, when nodes have no input yet, so amounts
  are recorded by `*Node` first and mirrored to the input outpoint as soon as
  one exists. Path extraction clones nodes but preserves their inputs, so
  `NodeAssetAmount` falls back to the outpoint map — that fallback is what
  makes an extracted client path still resolve its asset amounts. Do not drop
  either map.
- **`AssetTreeContext.Validate` requires a complete description.** Every node
  needs a non-zero asset amount and a 32-byte signing tweak; every *leaf*
  needs a 32-byte asset commitment root; every *branch* must have none; child
  asset totals may not exceed the parent's amount; and every node's input
  outpoint must be unique across the tree, because outpoint-keyed metadata
  would otherwise collide silently between nodes. Amount aggregation is
  overflow-checked at every level.
- **An asset tree's signing tweak comes from the context, not the sweep
  root.** When `Tree.AssetContext` is non-nil, signing uses
  `AssetContext.tweakLookup()` for each node's taproot tweak. A missing or
  wrong-length tweak must fail validation before any key derivation.
- **The asset context's getters and setters copy their byte slices.**
  `SigningTweak`, `LeafAssetRoot`, and `SealedPackage` return copies and store
  copies, so a caller cannot alias into the context. `cloneForRoot` makes an
  extracted path's context independent of the parent tree's. This is the same
  reasoning as the cache-aliasing invariant below — keep the copies.
- **Cache-aliasing invariant**: a `*Tree` is effectively immutable once published from
  a builder or resolver. Multiple downstream consumers may share the same `*Tree`
  pointer through caches and ancestry-fragment slices. Silently mutating a shared
  tree's nodes or root would corrupt every aliasing reader. Callers that need to
  transform a tree must clone it first.

## Deep Docs

- [lib/CLAUDE.md](../CLAUDE.md) — Parent lib package overview.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
