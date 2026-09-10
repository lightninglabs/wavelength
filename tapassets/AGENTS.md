# tapassets

## Purpose

Adapts tap-sdk custom-anchor asset transitions to Ark structures. Two entry
points: `NewBatchAnchorCommitter` moves confirmed Taproot Assets into a
caller-funded batch output, and `BuildAssetTree` materializes an asset-aware
VTXO tree beneath that batch output. Every tap-sdk commit is journaled before
it is used, so a restart replays the sealed package instead of re-committing.

## Key Types

For field-level detail, use `go doc github.com/lightninglabs/wavelength/tapassets.<Symbol>`.

### Batch anchor (`batch_anchor.go`)

- `BatchAnchorCommitter` — Creates caller-funded asset batch outputs.
  Three-step protocol: `DeriveScript` (compute the batch output script before
  Bitcoin funding), `Commit` (seal and validate the transition against the
  funded anchor PSBT), `Publish` (verify a finalized anchor PSBT and record it
  in tapd).
- `BatchAnchorRequest` — One transition moving confirmed assets into a single
  output: `AssetRef`, `Amount`, optional `Change`, `Sources`, `Cosigners`,
  `SweepLeaf`, `Digest`, `OutputIndex`, `OutputValueSat`. Output indexes must
  be final before deriving a request that carries change.
- `BatchAnchorScript` — Derived batch output material: `PkScript`,
  `InternalKey` (untweaked cosigner aggregate), `SigningTweak` (commits to
  the sweep leaf and asset root), `AssetRoot`, and `ChangePkScript`.
- `BatchAnchorSource` — One confirmed asset input: proof file, amount,
  optional witness (empty asks tapd to sign), verifier, anchor outpoint, and
  anchor internal key.
- `BatchAnchorChange` / `DeriveBatchAnchorChange` — Surplus assets returned to
  the operator's tapd wallet on its own anchor output.
- `BatchAnchorCommit` — Sealed transition plus the tree root input:
  `PackageBytes`, `AnchorPSBT`, the echoed `Script`, and `RootSource`.

### Asset tree (`tree_assembler.go`, `tree_materializer.go`)

- `AssetTreeRequest` — One asset-aware VTXO tree: `Leaves`, `OperatorKey`,
  `Radix`, `BatchOutpoint`, `BatchOutput`, `AssetAmount`.
- `BuildAssetTree` — Builds the tree, materializes every node through tap-sdk,
  and returns a `*tree.Tree` carrying a populated `tree.AssetTreeContext`.
- `TreeMaterializerConfig` — Materialization inputs: tap-sdk `Wallet`,
  journal `Store`, `AssetRef`, `SweepLeaf`, `LeafAnchor` hook, `Root`, and
  the `Digest` that scopes deterministic asset script keys.
- `TreeRootAssetSource` / `TreeRootAssetInput` — The asset states the root
  node spends, the batch output's taproot tweak, and its P2TR script. Exactly
  one of `ProofFile` and `ProofPath` must be set per input.
- `TreeLeafAnchor` / `StandardVTXOLeafAnchor` — Taproot data for a leaf VTXO
  output before the asset root is composed in.

### Journal (`journal.go`, `tree_journal.go`)

- `Store` — Two-method durable key/value interface (`Load`, `Store`) that
  persists sealed transfer packages. Implementations must replace each value
  atomically.
- `ErrStoreNotFound` — Sentinel returned by `Store.Load` for an absent key;
  the journal treats it as "not committed yet", not as an error.
- `ErrReconciliationRequired` — Returned by `Publish` when publication may
  have succeeded. The caller must check the outcome on chain before retrying.

## Relationships

- **Depends on**: `lib/tree` (tree structure, `AssetTreeContext`,
  `Materialize`), `lib/arkscript` (taproot policy composition for batch and
  leaf outputs), `lib/tx/psbtutil` (anchor PSBT handling), and the external
  `github.com/lightninglabs/tap-sdk` (`Wallet`, `CustomAnchorRequest`,
  `CustomAnchorTransferPackage`, proof paths).
- **Depended on by**: nothing yet. The package is wired in behind the asset
  VTXO request path in `round`; the `Store` implementation is supplied by the
  caller.
- **Sends / Receives**: no actor messages. The package is a synchronous
  library, driven by direct calls.

## Invariants

- **Commit is journaled, and the journal key is request-bound.** Every commit
  runs through `customAnchorCommitJournal`: the request is hashed under a
  domain-separated digest (`wavelength/asset-tree-request/v0` for trees), and
  a stored state whose `RequestDigest` differs from the current request is a
  hard error rather than a silent overwrite. A journaled package short-circuits
  the tap-sdk commit and is decoded instead, so replay after a crash cannot
  double-commit the same transition.
- **The journal write outlives the request context.** `storeStateAfterCommit`
  detaches with `context.WithoutCancel` and a 5 s timeout, because tapd has
  already mutated its state by the time the package is sealed: losing the
  write to a cancelled caller would strand the transition.
- **Publish failures may be non-terminal.** A failed `Publish` can still have
  landed. It reports `ErrReconciliationRequired`, and the caller must
  reconcile before retrying rather than treating it as a clean failure.
- **Anchor broadcast is external.** Commits are sealed with
  `SkipAnchorTxBroadcast` and `ExternalBroadcast`, so this package never
  broadcasts the anchor transaction — the caller funds, signs, and publishes
  it, then hands the finalized PSBT back through `Publish`.
- **Asset value is conserved twice over.** `buildAssetTree` requires that the
  leaf carrier values sum to exactly the batch output value (node transactions
  are zero fee, for v3 ephemeral-anchor relay), that the leaf asset amounts sum
  to exactly `AssetAmount`, and that the root asset inputs carry that same
  amount. Every leaf must carry a non-zero asset amount and a unique cosigner.
- **The batch output script is bound before materialization.**
  `req.BatchOutput.PkScript` must equal `cfg.Root.BatchPkScript`, so a tree can
  only be built against the batch output the root asset source actually
  describes.
- **Proof capacity is checked before building.** `validateTreeProofCapacity`
  rejects a tree whose depth would push a recursive `AssetProofPath` past
  tap-sdk's step, depth, or size bounds — the failure has to happen before any
  commit, not partway down the tree.
- **The materialized tree is re-verified.** `BuildAssetTree` calls
  `Tree.Verify` on the result, so an inconsistent materialization is rejected
  by the same structural and value checks the Bitcoin-only path uses.

## Deep Docs

- [lib/tree/CLAUDE.md](../lib/tree/CLAUDE.md) — Tree structure, asset context,
  and the materializer interface this package implements.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
