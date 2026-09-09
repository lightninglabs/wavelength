# tapassets

## Purpose

Adapts tap-sdk custom-anchor transitions to Ark trees. Given confirmed Taproot
Asset inputs, this package derives a caller-funded *batch anchor output* that
carries the assets, then materializes a full asset-aware VTXO tree beneath that
output — each node transaction being a tap-sdk custom-anchor transition whose
Bitcoin template is an Ark tree node.

The package is a pure library over `lib/tree` plus the external `tap-sdk`
wallet; it is not yet wired into the daemon and has no in-repo importers.

## Key Types

- `BatchAnchorCommitter` — Creates caller-funded asset batch outputs. Built by
  `NewBatchAnchorCommitter`; drives the three-step
  `DeriveScript` → `Commit` → `Publish` flow.
- `BatchAnchorRequest` — Moves confirmed assets into one caller-funded output:
  asset ref, amount, funding `Sources`, MuSig2 `Cosigners`, operator
  `SweepLeaf`, `Digest`, and the anchor transaction's `OutputIndex` /
  `OutputValueSat`.
- `BatchAnchorScript` — The derived batch output: composed `PkScript`, untweaked
  cosigner `InternalKey`, `SigningTweak` (commits to sweep leaf plus asset
  root), `AssetRoot`, and an optional `ChangePkScript`.
- `BatchAnchorCommit` — The sealed transition: `PackageBytes`, `AnchorPSBT`, the
  echoed `Script`, and the `RootSource` handed to tree materialization.
- `BatchAnchorSource` / `BatchAnchorChange` — One confirmed asset input, and the
  surplus returned to the operator's tapd wallet on its own anchor output
  (derived via `DeriveBatchAnchorChange`).
- `BuildAssetTree(ctx, TreeMaterializerConfig, AssetTreeRequest) (*tree.Tree, error)`
  — Builds and materializes an asset VTXO tree below a batch output.
- `AssetTreeRequest` — Leaves, operator key, radix, batch outpoint/output, and
  the tree's total `AssetAmount`.
- `TreeMaterializerConfig` — tap-sdk `Wallet`, journal `Store`, `AssetRef`,
  operator `SweepLeaf`, a `LeafAnchor` callback, the `Root` asset source, and
  the `Digest` that scopes deterministic asset script keys.
- `TreeRootAssetSource` / `TreeRootAssetInput` — The asset states the root node
  spends, plus the batch output's `SigningTweak` and `BatchPkScript`.
- `TreeLeafAnchor` — Taproot data for a leaf VTXO output (uncomposed pkScript,
  internal key, canonical tap leaves). `StandardVTXOLeafAnchor` produces one
  from an `arkscript` standard VTXO template.
- `Store` — Two-method (`Load`/`Store`) durable journal for sealed transition
  packages; `ErrStoreNotFound` reports an absent key.
- `ErrReconciliationRequired` — Publication outcome is unknown and must be
  reconciled before any retry.

## Relationships

- **Depends on**: `lib/tree` (structure building, materialization, cosigner and
  internal-key math, `AssetTreeContext`), `lib/arkscript` (leaf policy
  compilation for `StandardVTXOLeafAnchor`), `lib/tx/psbtutil` (anchor PSBT
  handling), and the external `github.com/lightninglabs/tap-sdk` wallet.
- **Depended on by**: nothing yet — the batch-anchor and asset-tree flows are
  built and tested standalone ahead of the daemon wiring.
- **Sends** / **Receives**: none. This package holds no actor; it is called
  synchronously by whichever component eventually owns asset rounds.

## Invariants

- **Value conservation is checked twice.** Leaf carrier values must sum exactly
  to the batch output's Bitcoin value (tree transactions are zero fee for v3
  ephemeral-anchor relay), and leaf asset amounts must sum exactly to both
  `AssetTreeRequest.AssetAmount` and the total carried by
  `TreeRootAssetSource.Inputs`. All three sums are overflow-checked.
- Every leaf must carry a non-zero asset amount and a unique cosigner key; a
  repeated cosigner is rejected before materialization.
- `AssetTreeRequest.BatchOutput.PkScript` must equal
  `TreeMaterializerConfig.Root.BatchPkScript` — the tree is refused if the
  output being spent is not the one the asset source describes.
- **Commits are journaled before they are trusted.** `commitDurably` keys each
  custom-anchor commit by a domain-separated SHA-256 digest of the request. A
  replay with a matching digest returns the journaled sealed package instead of
  re-committing; a *different* request under the same key is an error, not an
  overwrite. The journal write runs under `context.WithoutCancel` with its own
  timeout so a cancelled caller cannot lose a commit that already happened.
- `Publish` failures that carry a tap-sdk `CustomAnchorPublishAttemptError` with
  `OutcomeUnknown` are joined with `ErrReconciliationRequired`. Callers must
  reconcile the on-chain outcome before retrying — a blind retry can
  double-spend the asset inputs.
- Anchor broadcast is always external: `sdkDriver.Commit` sets
  `SkipAnchorTxBroadcast` and `ExternalBroadcast`, so tapd seals the transfer
  but never puts the anchor transaction on the wire. The caller funds, signs,
  and broadcasts it.
- Batch-anchor output indexes must be final before deriving a request with
  change; `DeriveScript` balances the template first.
- The materialized tree is run through `tree.Tree.Verify` before it is returned,
  so an inconsistent materialization fails closed rather than escaping.

## Deep Docs

- [lib/tree/CLAUDE.md](../lib/tree/CLAUDE.md) — Tree structure, materialization,
  and the `AssetTreeContext` this package populates.
- [lib/arkscript/CLAUDE.md](../lib/arkscript/CLAUDE.md) — Policy templates
  behind `StandardVTXOLeafAnchor`.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
