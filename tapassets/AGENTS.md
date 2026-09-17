# tapassets

## Purpose

Adapts tap-sdk custom-anchor transitions to Ark trees. It moves confirmed
Taproot Assets into a caller-funded batch output, then materializes an
asset-aware VTXO tree beneath that output, committing one tapd asset
transition per tree node. Bitcoin funding stays with the caller; this package
only derives the scripts, seals the asset transitions, and validates that what
tapd committed matches what was requested.

## Key Types

- `BatchAnchorCommitter` — Creates caller-funded asset batch outputs.
  `DeriveScript` computes the batch output pkScript before Bitcoin funding,
  `Commit` seals and validates the transition against a funded anchor PSBT, and
  `Publish` records a finalized anchor PSBT in tapd.
- `BatchAnchorRequest` / `BatchAnchorScript` / `BatchAnchorCommit` — The
  three-stage batch anchor payloads: what to move (`AssetRef`, `Amount`,
  `Sources`, `Cosigners`, `SweepLeaf`), the derived script material
  (`PkScript`, `InternalKey`, `SigningTweak`, `AssetRoot`), and the sealed
  result (`PackageBytes`, `AnchorPSBT`, `RootSource`).
- `BatchAnchorSource` / `BatchAnchorChange` — One confirmed asset input, and
  the surplus returned to the operator's tapd wallet on its own anchor output.
  `DeriveBatchAnchorChange` derives the wallet keys for the change output.
- `BuildAssetTree` / `AssetTreeRequest` — Builds an asset VTXO tree below a
  batch output from leaf descriptors, operator key, radix, and the batch
  outpoint/output.
- `TreeMaterializerConfig` — Wires the tap-sdk wallet, journal `Store`,
  `AssetRef`, sweep leaf, leaf-anchor callback, and root asset source used
  while materializing each node.
- `TreeRootAssetSource` / `TreeRootAssetInput` — The asset states held by the
  batch output that the tree root node spends.
- `TreeLeafAnchor` / `StandardVTXOLeafAnchor` — Taproot material for a leaf
  VTXO output: the uncomposed policy script, internal key, and canonical leaf
  ordering, before the asset commitment root is mixed in.
- `Store` — Durable key/value journal for sealed transition packages.
  Implementations must replace each value atomically.

## Relationships

- **Depends on**: `lib/tree` (node hierarchy, `LeafDescriptor`,
  `ComputeFinalKey`), `lib/arkscript` (taproot policy construction, anchor
  scripts), `lib/tx/psbtutil` (PSBT assembly for node templates), and the
  external `tap-sdk` wallet for custom-anchor commit/publish.
- **Depended on by**: nothing yet — this is foundation work landed ahead of
  the round-side wiring that will consume it.
- **Sends**: no actor messages; the package is a synchronous library.
- **Receives**: no actor messages.

## Invariants

- Tree node transactions pay zero fee: `AssetTreeRequest.BatchOutput.Value`
  must equal the sum of the leaf amounts, and the leaf asset amounts must sum
  exactly to `AssetTreeRequest.AssetAmount`. Every leaf must carry a non-zero
  asset amount.
- Output indexes must be final before deriving a `BatchAnchorRequest` that
  carries change — the change anchor script commits to its own output index.
- Commits are journaled before they are returned. The journal key is derived
  from the configured `Digest` and the node input outpoint
  (`asset-tree/<digest>/<outpoint>`), and the stored state pins a digest of
  the request. Reusing a key with a different request is rejected rather than
  silently overwritten, so a restart replays the same sealed package instead
  of producing a second transition.
- The journal write runs on a context detached from the caller
  (`context.WithoutCancel`) under a short timeout: a cancelled request must
  never lose the record of a transition tapd already sealed.
- `Publish` wraps a `tapsdk.CustomAnchorPublishAttemptError` whose outcome is
  unknown in `ErrReconciliationRequired`. Callers must check the on-chain and
  tapd outcome before retrying — a blind retry can double-spend the asset
  inputs.
- `Store.Load` must return `ErrStoreNotFound` (not a nil value) for an absent
  key; the journal treats any other error as fatal.
- Everything tapd commits is re-validated locally: committed inputs are
  matched against the requested sources, outputs against the derived script,
  and asset issuance conservation is checked before the commit is accepted.

## Deep Docs

- [lib/tree/CLAUDE.md](../lib/tree/CLAUDE.md) — VTXO tree construction and
  materialization this package layers assets onto.
- [lib/arkscript/CLAUDE.md](../lib/arkscript/CLAUDE.md) — Taproot policy
  compilation used for leaf and sweep scripts.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
