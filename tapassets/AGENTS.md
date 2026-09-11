# tapassets

## Purpose

Adapts tap-sdk custom-anchor transitions to Ark trees, so a VTXO tree can
carry Taproot Assets instead of only satoshis. Two things happen here: moving
confirmed assets into a single caller-funded *batch output*
(`BatchAnchorCommitter`), and materializing the asset-aware VTXO tree that
spends that batch output (`BuildAssetTree`). Each node transition is committed
through tapd and journaled so a crash mid-tree resumes on the same sealed
packages rather than re-deriving new ones.

## Key Types

- `BatchAnchorCommitter` — Creates caller-funded asset batch outputs across a
  three-call sequence: `DeriveScript` (compute the batch output script before
  Bitcoin funding), `Commit` (seal and validate the transition against the
  funded anchor PSBT), `Publish` (verify the finalized PSBT and record it in
  tapd). Built via `NewBatchAnchorCommitter(BatchAnchorCommitterConfig)`.
- `BatchAnchorRequest` / `BatchAnchorScript` / `BatchAnchorCommit` — The
  request describing the confirmed asset inputs and the intended batch output;
  the derived script (`PkScript`, untweaked `InternalKey`, `SigningTweak`,
  `AssetRoot`); and the sealed result (`PackageBytes`, `AnchorPSBT`, plus the
  `RootSource` that tree materialization consumes).
- `BatchAnchorSource` / `BatchAnchorChange` — One confirmed asset input (proof
  file, amount, anchor outpoint/internal key, optional pre-supplied witness),
  and the optional surplus output returning assets to the operator's tapd
  wallet. `DeriveBatchAnchorChange` derives the change output's wallet keys.
- `BuildAssetTree(ctx, TreeMaterializerConfig, AssetTreeRequest)` — Builds the
  asset VTXO tree beneath a batch output and returns a `lib/tree.Tree`.
- `TreeMaterializerConfig` / `AssetTreeRequest` — Materialization wiring (tapd
  `Wallet`, `Store`, `AssetRef`, operator `SweepLeaf`, `LeafAnchor` hook,
  `Root`, `Digest`) and the per-tree input (leaves, operator key, radix, batch
  outpoint/output, total asset amount).
- `TreeRootAssetSource` / `TreeRootAssetInput` — The asset states the root node
  spends, carried from `BatchAnchorCommit.RootSource` into materialization.
- `TreeLeafAnchor` / `StandardVTXOLeafAnchor(owner, operator, exitDelay)` — The
  taproot data for a leaf VTXO output: the policy script *before* the asset
  root is composed in, the internal key, and the canonically ordered tap
  leaves.
- `Store` — Two-method journal (`Load`/`Store` by string key) persisting
  completed transition packages. Implementations must replace each value
  atomically, and report a missing key as `ErrStoreNotFound`.
- `ErrReconciliationRequired` — Returned (joined with the cause) by `Publish`
  when tapd reports an unknown publication outcome, meaning the transfer may
  have landed and must be checked before any retry.

## Relationships

- **Depends on**: `lib/tree` (`Tree`/`Node`/`LeafDescriptor` — the tree being
  materialized), `lib/arkscript` (leaf policy construction and taproot
  helpers), `lib/tx/psbtutil` (PSBT encode/decode and signature attachment),
  `github.com/lightninglabs/tap-sdk` (custom-anchor tx builder, wallet, proof
  verifier, transfer package).
- **Depended on by**: nothing yet — the package is a standalone adapter
  library with no in-repo consumer. The `Store` interface therefore has **no
  production implementation**; wiring it into `db` and driving it from `round`
  is future work, so treat the asset-tree path as not yet reachable from the
  round protocol.
- **Sends**: no actor messages — this is a synchronous library, not an actor.
  All tapd interaction goes through the `tapsdk.Wallet` on the config.
- **Receives**: no actor messages; callers invoke the exported functions
  directly.

## Invariants

- **Node transactions are zero fee.** `AssetTreeRequest.BatchOutput.Value`
  must equal the sum of the leaf amounts, and the leaf asset amounts must sum
  to `AssetAmount` exactly. A mismatch fails tree construction.
- `AssetTreeRequest.Radix` must be at least 2; every leaf must carry a
  non-zero asset amount.
- **Output indexes must be final before `DeriveScript`.** The derived script
  commits to `BatchAnchorRequest.OutputIndex` (and the change output's index
  when change is present), so reordering anchor outputs after derivation
  invalidates the script. This is why the committer is split into three calls
  around the caller's Bitcoin funding step.
- **Commits are journaled, keyed, and digest-bound.** A batch anchor journals
  under `asset-batch/<digest>/<assetRef>/<outputIndex>`; a tree node journals
  under `asset-tree/<digest>/<inputOutpoint>`. Each stored state carries a
  SHA-256 digest of the request under a domain-separated prefix
  (`wavelength/asset-batch-request/v0`, `wavelength/asset-tree-request/v0`).
  Reusing a key with a *different* request is a hard error rather than a
  silent overwrite, and a replay with the same request returns the stored
  sealed package instead of committing again.
- **The journal write survives caller cancellation.** After tapd has committed,
  `storeStateAfterCommit` writes under `context.WithoutCancel` plus a 5s
  timeout, because losing the sealed package after tapd already moved the asset
  would strand the transition with no way to resume it.
- `Commit` requires tapd to report `CustomAnchorFundingCallerFundedExact`;
  any other funding mode is rejected, since the caller — not tapd — funds the
  anchor transaction.
- **`Publish` failures are not uniformly retryable.** A
  `tapsdk.CustomAnchorPublishAttemptError` with `OutcomeUnknown` is rewritten
  to join `ErrReconciliationRequired`; callers must reconcile before retrying,
  because a blind retry could double-spend the asset inputs.
- Commits are built with `SkipAnchorTxBroadcast` and `ExternalBroadcast` set:
  tapd seals and records the transition but never broadcasts. Broadcast is the
  caller's job, which is what makes the batch output caller-funded.
- `commitResponseError` distinguishes "tapd committed but local
  post-processing failed" from an ordinary commit failure — the former means
  tapd state has already advanced, so the error must not be treated as a
  clean no-op.
- Every committed output must carry a matching proof update; a missing proof
  blob fails package conversion rather than yielding a node with no proof.
- Journal state is size-bounded (`maxCustomAnchorCommitStateSize`, 128 MiB)
  and version-checked on decode; an unsupported version is a hard error.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
- [lib/tree/CLAUDE.md](../lib/tree/CLAUDE.md) — The tree model this package
  materializes assets into.
