# tapassets

## Purpose

Adapts tap-sdk *custom-anchor* transitions to Ark trees, so a VTXO tree can
carry Taproot Assets instead of only satoshis. Two stages: a
`BatchAnchorCommitter` moves confirmed assets into one caller-funded batch
output, then `BuildAssetTree` materializes an asset-carrying VTXO tree beneath
that output, committing one tap-sdk transition per tree node. Every commit is
journaled so a crash resumes from the sealed package rather than re-committing
against tapd.

## Key Types

For field-level detail, use
`go doc github.com/lightninglabs/wavelength/tapassets.<Symbol>`.

- `BatchAnchorCommitter` — creates caller-funded asset batch outputs. Three
  phases in order: `DeriveScript` (preview the commitment roots and compose the
  batch output script *before* Bitcoin funding), `Commit` (seal and validate
  the transition against the funded anchor PSBT), `Publish` (verify the
  finalized PSBT and record it in tapd).
- `BatchAnchorRequest` / `BatchAnchorSource` / `BatchAnchorChange` — the
  requested batch output (asset ref, amount, cosigners, sweep leaf, output
  index and Bitcoin value), its confirmed asset inputs, and the optional
  surplus returned to the operator's tapd wallet.
  `DeriveBatchAnchorChange` derives the change output's wallet keys.
- `BatchAnchorScript` — the derived batch output: composed `PkScript`,
  untweaked cosigner aggregate `InternalKey`, `SigningTweak` (commits to sweep
  leaf plus asset root), `AssetRoot`, and the optional `ChangePkScript`.
- `BatchAnchorCommit` — the sealed transition: `PackageBytes`, `AnchorPSBT`,
  the echoed `Script`, and the `RootSource` handed to tree materialization.
- `AssetTreeRequest` / `BuildAssetTree` — the leaves, operator key, radix, and
  batch outpoint/output for one asset tree, and the entry point that builds it.
- `TreeMaterializerConfig` — tap-sdk `Wallet`, `Store`, `AssetRef`,
  `SweepLeaf`, `LeafAnchor` resolver, `Root` (`TreeRootAssetSource`), and the
  `Digest` that scopes deterministic asset script keys.
- `TreeRootAssetSource` / `TreeRootAssetInput` — the asset states the root node
  spends, with the batch output's signing tweak and P2TR script. Each input
  carries exactly one of `ProofFile` (confirmed) or `ProofPath` (with
  unconfirmed transitions).
- `TreeLeafAnchor` / `StandardVTXOLeafAnchor` — the taproot material for a leaf
  VTXO output (uncomposed pkScript, internal key, canonical tap leaves), and the
  standard-VTXO-policy implementation built through `lib/arkscript`.
- `Store` — narrow durable journal (`Load`/`Store` by key) for sealed
  transition packages; implementations must replace each value atomically.
  `ErrStoreNotFound` reports a key with no durable value.
- `ErrReconciliationRequired` — joined onto a `Publish` failure whose outcome
  is unknown; the caller must check whether publication succeeded before
  retrying.

## Relationships

- **Depends on**: `lib/tree` (tree structure, `Materializer` interface,
  `AssetTreeContext`, value conservation), `lib/arkscript` (standard VTXO
  policy templates, `AnchorOutput`, `ARKNUMSKey`), `lib/tx/psbtutil` (anchor
  PSBT parse/serialize), and the external `github.com/lightninglabs/tap-sdk`
  (custom-anchor request/plan/commit, proof paths, verifiers).
- **Depended on by**: nothing yet. The package is a self-contained library; no
  repo package imports it and no production `Store` implementation exists, so
  the daemon wiring (which subsystem owns the committer and which table backs
  the journal) is still open.
- **Sends**: no actor messages. Every entry point is a synchronous call; tapd
  is reached through the tap-sdk `Wallet`, not through the actor system.
- **Receives**: no actor messages.

## Invariants

- **`DeriveScript` must run before Bitcoin funding, and output indexes must be
  final before deriving a request that carries change.** The batch output
  script commits to the previewed asset root, so the funding transaction can
  only be built once the script is known, and `Commit` rejects a funded anchor
  whose output at `req.OutputIndex` does not carry exactly the derived
  `PkScript` (and likewise for the change output).
- **Commits are journaled, and the journal key is single-use per request.**
  `customAnchorCommitJournal` digests the tap-sdk request under a versioned
  domain (`wavelength/asset-batch-request/v0` for batch anchors,
  `wavelength/asset-tree-request/v0` for tree nodes) and refuses a key that was
  already used with a *different* request. A key that already holds a sealed
  package decodes and returns it instead of re-committing against tapd.
- **The journal write survives caller cancellation.** `storeStateAfterCommit`
  wraps the request context in `context.WithoutCancel` plus a 5s timeout: tapd
  has already mutated its state by then, so losing the sealed package to a
  cancelled request context would strand the transition.
- **`Commit` holds `BatchAnchorCommitter.mu` across the journal check and the
  tapd mutation.** They are one critical section; concurrent commits on the
  same key would otherwise both miss the journal and double-commit.
- **Anchor transactions are committed with broadcast disabled**
  (`SkipAnchorTxBroadcast` + `ExternalBroadcast`). The caller owns broadcast:
  Ark tree transactions are relayed as v3 packages, not by tapd.
- **Node transactions are zero fee and value-conserving.** `buildAssetTree`
  requires the leaf carrier values to sum to exactly the batch output value and
  the leaf asset amounts to sum to exactly both `AssetAmount` and the root
  inputs' total, then re-checks the finished tree with `tree.Tree.Verify`.
- **Proof-path capacity is checked before any commit.**
  `validateTreeProofCapacity` rejects a root input whose proof path cannot
  absorb one transition step per tree level, against both
  `tapsdk.AssetProofPathMaxDepth` and `AssetProofPathMaxSize`. Discovering the
  ceiling mid-materialization would leave a partially committed tree.
- **Asset script keys are deterministic, not random.**
  `deterministicKey` derives them from `TreeMaterializerConfig.Digest` (or
  `BatchAnchorRequest.Digest`) under the `wavelength/assets/optrue/v0/` domain,
  so a resumed materialization reproduces byte-identical requests and the
  journal digest still matches.
- **Every commit response is re-validated locally.** `validateCommit`,
  `validateBatchInputs`, `validateCommittedOutputIDs`, and
  `checkCommittedChange` bind tapd's committed inputs/outputs back to what was
  requested; `boundFinalKey` checks the node's cosigner aggregate against the
  output actually being spent before signing material is recorded. A failure
  after tapd returned a committed response surfaces as `commitResponseError`,
  which is a local-processing failure, not a rejected transition.
- **`ErrReconciliationRequired` is not retryable.** It is returned only when
  tap-sdk reports `OutcomeUnknown`, meaning publication may have landed;
  blindly retrying could double-publish.
- **Cosigner slices are never sorted in place here.** The materializer relies
  on `lib/tree`'s copy-before-aggregate contract; see the same invariant in
  [`lib/tree/CLAUDE.md`](../lib/tree/CLAUDE.md).

## Deep Docs

- [lib/tree/CLAUDE.md](../lib/tree/CLAUDE.md) — tree structure,
  `AssetTreeContext`, and value conservation.
- [lib/arkscript/CLAUDE.md](../lib/arkscript/CLAUDE.md) — policy templates and
  external root composition.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
