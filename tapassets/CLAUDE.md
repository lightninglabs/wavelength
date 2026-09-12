# tapassets

## Purpose

Adapts tap-sdk custom-anchor transitions to Ark VTXO trees, so a batch output
can carry Taproot Asset units alongside its Bitcoin carrier value. The package
does two things: it moves confirmed assets into one caller-funded batch output
(`BatchAnchorCommitter`), and it materializes an asset-aware VTXO tree beneath
that output (`BuildAssetTree`), committing one tap-sdk transition per node.
Every commit is journaled so a restart replays the sealed package instead of
re-committing a transition tapd has already recorded.

## Key Types

- `BatchAnchorCommitter` — Creates caller-funded asset batch outputs in three
  phases: `DeriveScript` (compute the batch output script *before* Bitcoin
  funding, so the funder knows what to pay to), `Commit` (seal and validate the
  transition against the funded anchor transaction), and `Publish` (verify the
  finalized PSBT and record it in tapd). Constructed via
  `NewBatchAnchorCommitter(BatchAnchorCommitterConfig{Wallet, Store})`.
- `BatchAnchorRequest` — One caller-funded batch output: `AssetRef`, `Amount`,
  confirmed `Sources`, `Cosigners` (aggregated into the output's internal key),
  the operator `SweepLeaf` timeout path, a `Digest` scoping the deterministic
  asset script key, the `OutputIndex`/`OutputValueSat` of the batch output, and
  an optional `Change` output returning surplus assets to the tapd wallet.
- `BatchAnchorScript` — Derived batch output: `PkScript`, untweaked
  `InternalKey`, `SigningTweak` (commits to sweep leaf + asset root),
  `AssetRoot`, and `ChangePkScript` when the request carries change.
- `BatchAnchorCommit` — Sealed transition: `PackageBytes`, `AnchorPSBT`, the
  echoed `Script`, and the `RootSource` the tree materializer spends.
- `BatchAnchorSource` / `BatchAnchorChange` / `DeriveBatchAnchorChange` — One
  confirmed asset input (proof file, amount, optional witness, verifier, anchor
  outpoint/internal key), and the wallet-owned change output plus its key
  derivation helper.
- `BuildAssetTree(ctx, TreeMaterializerConfig, AssetTreeRequest)` — Builds the
  asset VTXO tree below a batch output. Validates leaf/asset conservation,
  calls `tree.BuildStructure` + `tree.Materialize` with the internal
  `treeMaterializer`, and returns a verified `*tree.Tree` carrying an
  `AssetContext`.
- `AssetTreeRequest` — Tree inputs: `Leaves` (each with a non-zero
  `AssetAmount`), `OperatorKey`, `Radix`, `BatchOutpoint`, `BatchOutput`, and
  the total `AssetAmount`.
- `TreeMaterializerConfig` — Materialization wiring: tap-sdk `Wallet`, commit
  journal `Store`, `AssetRef`, operator `SweepLeaf`, a `LeafAnchor` callback
  returning per-leaf taproot data, the `Root` asset source, and the `Digest`
  scoping deterministic script keys.
- `TreeRootAssetSource` / `TreeRootAssetInput` — The asset states the root node
  spends, plus the batch output's `SigningTweak` and `BatchPkScript`. Each
  input carries **exactly one** of `ProofFile` (confirmed) or `ProofPath`
  (unconfirmed transitions).
- `TreeLeafAnchor` / `StandardVTXOLeafAnchor` — Taproot data for a leaf VTXO
  output (uncomposed pkScript, internal key, canonically ordered tap leaves);
  the helper builds it for a standard owner/operator + exit-delay VTXO policy
  via `lib/arkscript`.
- `Store` / `ErrStoreNotFound` — Durable commit journal: `Load`/`Store` keyed by
  string. Implementations must replace each value **atomically**.
  `ErrStoreNotFound` is the sentinel a missing key must return.
- `ErrReconciliationRequired` — Publication may have succeeded; the outcome must
  be checked before any retry.

## Relationships

- **Depends on**: `lib/tree` (`Tree`, `Node`, `LeafDescriptor`,
  `BuildStructure`, `Materialize`, and the tree `AssetContext`),
  `lib/arkscript` (VTXO policy templates and compiled taproot leaves),
  `lib/tx/psbtutil` (anchor PSBT parse/encode),
  `github.com/lightninglabs/tap-sdk` (custom-anchor wallet, transfer packages,
  proof verifiers).
- **Depended on by**: nothing yet — `tapassets` is a standalone building block.
  It is consumed through the asset-tree plumbing landing in `round` and
  `rpc/roundpb` (`VTXOTree.asset_ref`, `TreeNode.signing_tweak` /
  `asset_amount` / `asset_commitment_root`, and
  `ClientBatchInfo.asset_leaf_packages`), not by importing this package
  directly. Wire a new consumer through `TreeMaterializerConfig` /
  `BatchAnchorCommitterConfig` rather than reaching at internals.
- **Sends / Receives**: none. This package holds no actor; it is called
  synchronously and performs no message passing.

## Invariants

- **Node transactions are zero fee.** The leaf carrier values must sum exactly
  to `BatchOutput.Value`, and the leaf asset amounts must sum exactly to
  `AssetTreeRequest.AssetAmount`, which must in turn equal the sum of the root
  source input amounts. All three totals are overflow-checked before use.
- Each leaf cosigner key must be unique within a tree; a repeated cosigner is
  rejected rather than silently collapsed.
- `req.BatchOutput.PkScript` must equal `cfg.Root.BatchPkScript` — the tree is
  refused if the output being spent is not the one the asset source describes.
- `Radix` must be at least 2, and proof capacity is validated against the built
  structure's depth (`validateTreeProofCapacity`) before any transition is
  committed, so a tree too deep for its proof budget fails early.
- **The commit journal is keyed by request digest, not just by key.** A journal
  entry records the sha256 of `(digestDomain, request)`; reloading a key with a
  *different* request is a hard error ("journal key reused with a different
  request") rather than a silent re-commit. A present package is decoded and
  returned as-is — tapd has already recorded that transition and must not see
  it twice.
- Journal writes after a successful commit run under
  `context.WithoutCancel` with a 5s timeout (`commitJournalWriteTimeout`), so a
  cancelled caller cannot lose the record of a transition tapd already
  committed.
- `ErrReconciliationRequired` is joined onto a `Publish` failure **only** when
  tap-sdk reports `OutcomeUnknown`. Treat it as "may have landed": reconcile
  before retrying, never blind-retry.
- `Commit` binds the sealed transition to the funded transaction: the committed
  anchor PSBT's txid must match the funded transaction's, and the committed
  batch outpoint must equal `(finalTx.TxHash(), req.OutputIndex)`. Output
  indexes must therefore be final before deriving a request with change.
- `commitResultFromPackage` requires every output to have a matching proof
  update, keyed by `(packetRole, packetIndex, virtualOutputIndex)`; an output
  with no proof is a hard error, not an empty blob.
- A failure converting an already-committed tapd response is wrapped in
  `commitResponseError` so callers can tell "tapd never committed" from "tapd
  committed and we failed locally afterwards" — the latter needs the journal,
  not a retry.
- All byte slices crossing the package boundary (`packageBytes`, `anchorPSBT`,
  proof blobs) are defensively copied, so a caller cannot mutate journaled
  state.

## Deep Docs

- [lib/tree/CLAUDE.md](../lib/tree/CLAUDE.md) — Tree structure, materialization
  interface, and the asset context this package populates.
- [lib/arkscript/CLAUDE.md](../lib/arkscript/CLAUDE.md) — Policy templates and
  compiled taproot leaves used for leaf anchors.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
