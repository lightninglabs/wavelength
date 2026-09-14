# tapassets

## Purpose

Adapts tap-sdk custom-anchor transitions to Ark VTXO trees. Moves confirmed
Taproot Assets into a caller-funded batch output, then materializes an
asset-aware VTXO tree beneath that output so every tree node carries a valid
asset transition alongside its Bitcoin transaction.

## Key Types

- `BatchAnchorCommitter` — Creates caller-funded asset batch outputs. `DeriveScript` computes the batch pkScript for a request, `Commit` seals the transition against a funded PSBT, and `Publish` hands the finished package to tapd.
- `BatchAnchorRequest` — Describes one batch output: asset ref, total amount, confirmed funding sources, cosigner set, sweep leaf, and the output's final index/value.
- `BatchAnchorChange` — Optional surplus output returning funding change to the operator's tapd wallet; built via `DeriveBatchAnchorChange`.
- `BuildAssetTree` / `TreeMaterializerConfig` — Entry point and configuration for materializing an asset VTXO tree below a batch output.
- `AssetTreeRequest` — Leaves, operator key, radix, and batch outpoint for one asset-aware tree.
- `TreeLeafAnchor` — Taproot material (uncomposed pkScript, internal key, tap leaves) for a leaf VTXO output; `StandardVTXOLeafAnchor` builds the standard owner/operator form.
- `TreeRootAssetSource` / `TreeRootAssetInput` — The asset states held by the batch output that the tree root spends, plus the batch taproot tweak and pkScript.
- `Store` — Durable journal for completed transition packages; implementations must replace each value atomically.

## Relationships

- **Depends on**: `lib/tree` (node hierarchy, leaf descriptors, materialization),
  `lib/arkscript` (taproot policy construction, sweep/exit leaves),
  `lib/tx/psbtutil` (PSBT encoding and signature attachment), and the external
  `tap-sdk` custom-anchor API.
- **Depended on by**: nothing yet — this is foundation code staged ahead of the
  round-level asset wiring. `round` and `db` carry the asset request and state
  plumbing (`round.AssetVTXORequest`, `lib/tree.AssetTreeContext`) that will
  drive this package.
- **Sends** / **Receives**: no actor messages. This package is a synchronous
  library called from tree construction, not an actor.

## Invariants

- Asset value is conserved across every transition: a node's asset outputs must
  sum to its asset inputs, and the batch output's funding sources must carry
  exactly `BatchAnchorRequest.Amount` plus any declared change.
- Output indexes must be final before deriving a `BatchAnchorRequest` with
  change — the change derivation binds to the anchor transaction's output
  layout.
- Every leaf in an `AssetTreeRequest` must carry a non-zero asset amount.
  Asset-bearing trees may not contain value-free leaves.
- `ErrReconciliationRequired` means publication may have already succeeded. The
  caller must check the on-chain outcome before retrying; blind retry risks a
  double-publish.
- Commits are journaled before publication and keyed by a deterministic request
  digest, so a restart replays the same package rather than rebuilding a
  divergent one. `ErrStoreNotFound` distinguishes "no prior attempt" from a
  load failure.
- Exactly one of `TreeRootAssetInput.ProofFile` and `ProofPath` must be set.
- Script keys and request digests are domain-separated
  (`wavelength/asset-tree-request/v0`, `wavelength/asset-batch-request/v0`,
  `wavelength/assets/optrue/v0/`). Changing a domain string invalidates every
  previously journaled commit.

## Deep Docs

- [lib/tree/CLAUDE.md](../lib/tree/CLAUDE.md) — Tree construction and the
  `AssetTreeContext` that carries asset state through a tree.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
