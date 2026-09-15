# tapassets

## Purpose

Adapts tap-sdk custom-anchor transitions to Ark trees. Given confirmed asset
inputs, it derives and commits the caller-funded batch anchor output that
carries a round's assets, then materializes an asset-aware VTXO tree beneath
that batch output so every node transaction carries its share of the asset
commitment alongside the Bitcoin value.

## Key Types

- `BatchAnchorCommitter` — Creates caller-funded asset batch outputs. Three-step
  flow: `DeriveScript` (derive the batch output script from a template),
  `Commit` (seal the transition against a funded PSBT), `Publish` (hand the
  sealed package and final PSBT to tapd).
- `BatchAnchorRequest` / `BatchAnchorSource` / `BatchAnchorChange` — Input
  description for one batch anchor: the asset ref and amount, the confirmed
  asset inputs (proof file, amount, anchor outpoint, internal key, verifier),
  and the optional surplus-returning change output owned by the tapd wallet.
- `BatchAnchorScript` / `BatchAnchorCommit` — The derived batch output script
  and the sealed transition package produced by `Commit`.
- `BuildAssetTree` / `AssetTreeRequest` — Builds an asset VTXO tree below a
  batch output from leaf descriptors, operator key, radix, and the batch
  outpoint/output.
- `TreeMaterializerConfig` — Wiring for tree materialization: tap-sdk `Wallet`,
  journal `Store`, `AssetRef`, operator `SweepLeaf`, per-leaf `LeafAnchor`
  callback, `Root` asset source, and the `Digest` that scopes deterministic
  asset script keys.
- `TreeLeafAnchor` / `StandardVTXOLeafAnchor` — Taproot anchor material
  (uncomposed pkScript, internal key, tap leaves) for a tree leaf;
  `StandardVTXOLeafAnchor` derives it from the standard VTXO policy template.
- `TreeRootAssetSource` / `TreeRootAssetInput` — Identifies the asset spent by
  the tree's root node.
- `Store` — Journal interface (`Load`/`Store` by key) persisting completed
  transition packages. Implementations must replace each value atomically;
  a missing key must return `ErrStoreNotFound`.

## Relationships

- **Depends on**: `lib/tree` (tree structure, `Node`, `LeafDescriptor`,
  materialization), `lib/arkscript` (standard VTXO policy templates and
  taproot compilation for leaf anchors), `lib/tx/psbtutil` (PSBT encoding and
  signature attachment), and the external `tap-sdk` (custom-anchor requests,
  wallet, proof verification).
- **Depended on by**: nothing yet — the package is a self-contained adapter
  layer that is not wired into the daemon. Asset round integration is expected
  to call it from the round/batch construction path.
- **Sends**: no actor messages; this is a synchronous library, not an actor.
- **Receives**: no actor messages.

## Invariants

- **Value conservation**: `AssetTreeRequest.BatchOutput` value must equal the
  sum of the leaf amounts, and the leaf amounts must sum exactly to
  `AssetAmount`. Node transactions are zero fee, matching `lib/tree`'s v3
  ephemeral-anchor relay model. Every leaf must carry a non-zero asset amount.
- **Batch anchor funding**: `BatchAnchorRequest` funding sources must carry
  exactly `Amount` plus any change. Output indexes must be final before
  deriving a request that has a change output — the change derivation binds to
  its anchor output position.
- **Commit journaling is keyed and digest-guarded**: every commit is journaled
  under a key scoped by `Digest` and the spent outpoint. The journal records a
  SHA-256 digest of the request under a domain separator
  (`wavelength/asset-tree-request/v0`); reusing a key with a different request
  is rejected rather than silently re-committing. A journaled package is
  decoded and replayed instead of re-committing, which is what makes the flow
  restart-safe.
- **Journal writes survive request cancellation**: the post-commit journal
  write runs under `context.WithoutCancel` with its own 5s timeout. A commit
  that succeeded must never be lost because the caller's request context was
  cancelled — that would strand a committed transition with no journal record.
- **`ErrReconciliationRequired` is not a retry signal**: it reports that
  publication may have succeeded. Callers must check the outcome on-chain
  before retrying, never blindly re-publish.
- Deterministic asset script keys are derived from `Digest` plus a domain
  string, so the same request always produces the same keys — this is what
  makes replay after restart converge on the identical transition.

## Deep Docs

- [lib/tree/CLAUDE.md](../lib/tree/CLAUDE.md) — VTXO tree construction and
  materialization this package layers assets onto.
- [lib/arkscript/CLAUDE.md](../lib/arkscript/CLAUDE.md) — Policy templates used
  to derive leaf anchors.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
