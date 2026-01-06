# Asset/BTC Tree Architecture

This document describes the current `lib/tree` design after the two‑pass
refactor. The tree package now supports both BTC‑only trees and asset‑aware
trees while keeping the signing API consistent.

## Overview

Tree construction is split into two passes:

1) **Structure pass** (`PlanStructure`)
   - Inputs: `[]LeafDescriptor`, radix, weight function, operator key.
   - Outputs: `TreeStructure{Root, AssetContext, LeafScriptMap}`.
   - Populates the tree shape (cosigners, children), sets `Node.Amount` to the
     leaf BTC amount and aggregates subtree totals for branches.
   - Asset data is stored in the AssetContext; BTC leaf scripts are stored in
     the `LeafScriptMap`.

2) **Materialization pass** (`Materialize`)
   - Inputs: the root node, root params (`MaterializeParams`), and a
     materializer.
   - Outputs: each node is filled with its `Input`, `Outputs`, and `FinalKey`.
   - Implementations:
     - `BTCMaterializer`: builds BTC transactions using `Node.Amount` and the
       `LeafScriptMap`; ignores asset fields.
     - `AssetMaterializer`: builds asset transactions via tapd and uses the
       AssetContext (proofs, tweaks, metadata).

Signing remains unchanged: `NewTreeSignerSession` takes a tweak lookup (asset
trees use `TweakLookupFromAssetContext`; BTC trees pass `nil`).

## Key Data Structures

- `Node`
  - Core fields: `Input`, `Outputs`, `CoSigners`, `Children`, `Amount`
    (subtree BTC total), `Signature`, `FinalKey`.
  - Asset‑specific state is not stored on `Node`.

- `TreeStructure`
  - `Root`: root node of the structured tree (no outputs yet).
  - `AssetContext`: asset‑only state (asset amounts, proofs, leaf metadata,
    tweaks); used by the asset materializer and signing tweaks.
  - `LeafScriptMap`: BTC‑only map of leaf node pointers to pkScripts; used by
    the BTC materializer.

- `MaterializeParams`
  - Common: `Input`.
  - Asset‑only: `ParentProof`, `ParentPlan` (ignored by BTC).

## Assemblers

- `BTCTreeAssembler`
  - `BuildTree(ctx, rootInput, rootOutput, leaves)`:
    - Runs `PlanStructure` with BTC weighting/radix defaults.
    - Sanity checks that the sum of leaf amounts (`root.Amount`) does not
      exceed `rootOutput.Value`.
    - Materializes with `BTCMaterializer` using `LeafScriptMap`.
    - Returns `*Tree` (no asset context needed for signing).

- `AssetTreeAssembler`
  - `BuildTree(ctx, rootInput, rootPlan, rootProof, rootOutput, leaves, radix)`:
    - Runs `PlanStructure` with asset weighting.
    - Materializes with `AssetMaterializer`, passing the AssetContext.
    - Returns `*Tree` and the AssetContext; callers must keep them paired for
      signing (`TweakLookupFromAssetContext`) and finalization.

## Value Semantics

- BTC amounts:
  - `LeafDescriptor.Amount` is copied to `Node.Amount` at leaves.
  - Branch `Node.Amount` is the sum of descendant leaf amounts.
  - BTC materialization uses these amounts directly; no top‑down splitting.
  - `BTCTreeAssembler` errors if leaf sums exceed the root output value
    (root may exceed leaf sums if external fees/change are expected).

- Asset data:
  - Stored only in the AssetContext (proofs, asset amounts, labels, funding,
    tweaks). BTC code does not touch it.

## Signing

- BTC trees: `NewTreeSignerSession` with `tweakLookup=nil`.
- Asset trees: `TweakLookupFromAssetContext(assetCtx)` supplies per‑node tweaks
  for MuSig2 sessions.

## Compatibility Notes

- `NewTreeWithConfig` now delegates to `BTCTreeAssembler` (BTC path). It uses
  `context.Background()` internally; use the assemblers directly if you need an
  explicit context.
- Legacy `buildTreeBFS`/single‑pass construction is superseded by the two‑pass
  structure/materialize flow.
