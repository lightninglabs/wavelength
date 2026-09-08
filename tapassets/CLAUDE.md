# tapassets

## Purpose

Adapts tap-sdk custom-anchor asset transitions onto Ark's virtual transaction
trees, so a VTXO tree can carry Taproot Assets alongside its Bitcoin carrier
value. It does two jobs: move confirmed assets into a single caller-funded
**batch output** (`BatchAnchorCommitter`), and materialize the asset
transitions for every node of the VTXO tree that spends that batch output
(`BuildAssetTree`).

## Key Types

For field-level detail, use `go doc github.com/lightninglabs/wavelength/tapassets.<Symbol>`.

- `BatchAnchorCommitter` — creates caller-funded asset batch outputs.
  `DeriveScript` returns the composed batch output script before the anchor
  transaction is funded, `Commit` seals the transition once the funded PSBT
  exists, and `Publish` verifies and records the finalized anchor
  transaction. Constructed with `NewBatchAnchorCommitter`.
- `BatchAnchorRequest` / `BatchAnchorSource` / `BatchAnchorChange` — the
  request to move confirmed asset inputs into one batch output, its
  confirmed asset inputs, and the optional surplus returned to the
  operator's tapd wallet on its own anchor output.
- `BatchAnchorScript` — the derived batch output script plus its untweaked
  cosigner aggregate internal key, `SigningTweak` (commits to the sweep leaf
  and the asset root), and `AssetRoot`.
- `BatchAnchorCommit` — the sealed transfer package, the anchor PSBT carrying
  tapd's input signatures, the echoed `BatchAnchorScript`, and the
  `TreeRootAssetSource` that feeds tree materialization.
- `BuildAssetTree(ctx, TreeMaterializerConfig, AssetTreeRequest) (*tree.Tree, error)`
  — builds an asset VTXO tree beneath a batch output, returning a
  `lib/tree.Tree` with its `AssetContext` populated.
- `TreeMaterializerConfig` / `AssetTreeRequest` — materialization config (tapd
  wallet, `Store`, `AssetRef`, sweep leaf, `LeafAnchor` callback, root asset
  source, script-key `Digest`) and the per-tree request (leaves, operator key,
  radix, batch outpoint/output, total asset amount).
- `TreeLeafAnchor` / `StandardVTXOLeafAnchor` — the taproot data for a leaf
  VTXO output (uncomposed pkScript, internal key, canonical tap leaves), and
  the standard owner/operator + exit-delay construction of it.
- `TreeRootAssetSource` / `TreeRootAssetInput` — the asset states the tree's
  root node spends, the batch output's taproot tweak, and its P2TR script.
- `Store` — persists sealed transition packages; implementations must replace
  each value atomically.
- `ErrReconciliationRequired` — publication may have succeeded and its
  outcome must be checked before retrying.
- `ErrStoreNotFound` — sentinel returned by `Store.Load` for a missing key.

## Relationships

- **Depends on**: `lib/tree` (`Tree`, `Node`, `LeafDescriptor`,
  `BuildStructure`, `Materialize`, `AssetTreeContext`), `lib/arkscript`
  (policy compilation, `ComposeWithSiblingRoot` for folding the asset root
  into a leaf's taproot output), `lib/tx/psbtutil` (anchor PSBT handling),
  `github.com/lightninglabs/tap-sdk` (custom-anchor builder, transfer
  packages, proof verification).
- **Depended on by**: nothing yet. The package is the asset-side adapter
  built ahead of its daemon wiring; the consumer seam is `round`'s
  `AssetVTXOVerifier` and the `tapassets.Store` implementation the daemon
  will supply.
- **Sends / Receives**: no actor messages. This is a synchronous library
  called on the caller's goroutine, not an actor.

## Invariants

- **Node transactions are zero fee.** `AssetTreeRequest.BatchOutput.Value`
  must equal the sum of the leaf carrier amounts exactly, and the leaf asset
  amounts must sum to both `AssetAmount` and the total carried by
  `TreeRootAssetSource.Inputs`. Value and asset conservation are checked
  before any transition is committed.
- **The batch output script is bound to the root asset source.**
  `buildAssetTree` rejects a request whose `BatchOutput.PkScript` differs
  from `cfg.Root.BatchPkScript`, so a tree can never be materialized against
  a batch output the asset transitions do not commit to.
- **Every leaf carries a distinct cosigner and a non-zero asset amount.**
  Repeated cosigner keys, zero asset amounts, non-positive carrier values,
  and a radix below 2 are all rejected up front.
- **Commits are journaled before they are relied on.**
  `customAnchorCommitJournal.commitDurably` keys on the request digest
  (`sha256(domain || json(request))`) and stores the sealed package under
  `asset-tree/<digest>/<input>`. A replay with a byte-identical request
  decodes the journaled package instead of re-committing; a **different**
  request under the same key is a hard error ("journal key reused with a
  different request"), never a silent overwrite. Journal writes use
  `context.WithoutCancel` plus a 5s timeout, so a cancelled caller cannot
  lose the record of a commit tapd already performed.
- **Publication is not idempotent-by-retry.** When `Publish` may have
  succeeded, the caller gets `ErrReconciliationRequired` and must check the
  outcome on chain before retrying. Do not wrap that error in a generic
  retry loop.
- **Proof capacity is validated against tree depth before materializing.**
  `validateTreeProofCapacity` rejects a root asset input whose existing proof
  path depth plus the tree's depth would exceed
  `tapsdk.AssetProofPathMaxDepth`, or whose confirmed proof file exceeds
  `tapsdk.AssetProofPathMaxConfirmedProofSize`. Materializing first and
  discovering the overflow later would strand assets in a tree whose leaves
  cannot produce a valid proof.
- **Exactly one of `TreeRootAssetInput.ProofFile` and `ProofPath` is set** —
  a confirmed proof file, or a path proving a state with unconfirmed
  transitions.
- **Output indexes must be final before deriving a request with change.**
  `BatchAnchorRequest.OutputIndex` and `Change` commit to positions in the
  anchor transaction; deriving the script against a draft ordering produces a
  script that does not match the transaction that is eventually funded.
- **An empty witness means "tapd signs".** Both `BatchAnchorSource.Witness`
  and `TreeRootAssetInput.Witness` treat an empty stack as a request for tapd
  to sign the asset input, not as an unsigned error.
- The built tree is run through `tree.Tree.Verify()` before it is returned,
  so a materialization that produced an inconsistent tree fails here rather
  than at signing time.

## Deep Docs

- [lib/tree/CLAUDE.md](../lib/tree/CLAUDE.md) — Tree structure, materialization,
  and the `AssetTreeContext` this package populates.
- [round/CLAUDE.md](../round/CLAUDE.md) — Client-side asset VTXO request,
  verification, and signing path.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
