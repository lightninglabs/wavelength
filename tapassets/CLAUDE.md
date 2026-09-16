# tapassets

## Purpose

Adapts tap-sdk custom-anchor transitions to Ark trees. It moves confirmed
Taproot Assets into a caller-funded *batch output* whose script is the ordinary
Ark cosigner aggregate, then materializes a VTXO tree beneath that output so
every node transaction carries an asset transition alongside its Bitcoin spend.
This is the asset-aware counterpart to the Bitcoin-only tree construction in
`lib/tree`.

## Key Types

For field-level detail, use
`go doc github.com/lightninglabs/wavelength/tapassets.<Symbol>`.

### Batch anchor (`batch_anchor.go`)

- `BatchAnchorCommitter` — Creates caller-funded asset batch outputs across
  three ordered steps: `DeriveScript` (compute the batch output script before
  Bitcoin funding), `Commit` (seal and validate the transition against a funded
  anchor PSBT), and `Publish` (verify a finalized anchor PSBT and record it in
  tapd). Constructed with `NewBatchAnchorCommitter(BatchAnchorCommitterConfig)`.
- `BatchAnchorRequest` — Moves confirmed assets into one caller-funded output:
  `AssetRef`, `Amount`, optional `Change`, `Sources`, `Cosigners`, `SweepLeaf`,
  `Digest`, `OutputIndex`, `OutputValueSat`.
- `BatchAnchorScript` — Derived batch output script and its roots: `PkScript`,
  `InternalKey` (the untweaked cosigner aggregate), `SigningTweak` (commits to
  the sweep leaf and asset root), `AssetRoot`, and `ChangePkScript`.
- `BatchAnchorCommit` — Sealed transition plus its tree root input:
  `AssetRef`, `OutputIndex`, `PackageBytes`, `AnchorPSBT`, `Script`,
  `RootSource`.
- `BatchAnchorSource` — One confirmed asset input (`ProofFile`, `Amount`,
  `Witness`, `Verifier`, `AnchorOutpoint`, `AnchorInternalKey`). An empty
  `Witness` asks tapd to sign the asset input.
- `BatchAnchorChange` / `DeriveBatchAnchorChange` — Returns surplus assets to
  the operator's tapd wallet on its own anchor output.

### Tree materialization (`tree_assembler.go`, `tree_materializer.go`)

- `BuildAssetTree(ctx, TreeMaterializerConfig, AssetTreeRequest) (*tree.Tree, error)`
  — Builds an asset VTXO tree below a batch output. Validates the request,
  delegates layout to `tree.BuildStructure`, materializes each node through the
  tap-sdk driver, and returns a `*tree.Tree` with a populated
  `tree.AssetTreeContext`.
- `AssetTreeRequest` — One asset-aware tree: `Leaves` (`tree.LeafDescriptor`,
  each with a non-zero `AssetAmount`), `OperatorKey`, `Radix`, `BatchOutpoint`,
  `BatchOutput`, `AssetAmount`.
- `TreeMaterializerConfig` — Wiring: `Wallet` (tap-sdk), `Store`, `AssetRef`,
  `SweepLeaf`, `LeafAnchor` hook, `Root` (`TreeRootAssetSource`), `Digest`.
- `TreeRootAssetSource` / `TreeRootAssetInput` — The asset states the root node
  spends, plus the batch output's `SigningTweak` and `BatchPkScript`. Exactly
  one of `ProofFile` (confirmed) and `ProofPath` (unconfirmed transitions) must
  be set per input.
- `TreeLeafAnchor` / `StandardVTXOLeafAnchor(owner, operator, exitDelay)` —
  Taproot data for a leaf VTXO output: the policy script *before* the asset
  root is composed in (`UncomposedPkScript`), the `InternalKey`, and the
  canonical `TapLeaves`. Derived through `lib/arkscript` so asset leaves and
  Bitcoin-only leaves share one policy definition.

### Durable journal (`journal.go`, `tree_journal.go`)

- `Store` — `Load`/`Store` over opaque byte values keyed by string.
  Implementations must replace each value atomically.
- `ErrStoreNotFound` — Sentinel for a journal key with no durable value;
  treated as "first attempt", not an error.
- `ErrReconciliationRequired` — Publication may have succeeded and its outcome
  must be checked before retrying.

## Relationships

- **Depends on**: `lib/tree` (tree layout, `LeafDescriptor`,
  `AssetTreeContext`, `Materialize`), `lib/arkscript` (leaf policy templates
  and taproot compilation), `lib/tx/psbtutil` (PSBT encode/decode and
  signature attachment), `github.com/lightninglabs/tap-sdk` (custom-anchor
  transitions, wallet, proof verifiers).
- **Depended on by**: nothing in-repo yet. The package is the adapter layer
  the round asset path is being built against; the producing call sites
  (`round` tree construction, `db` asset state) are not wired to it yet, so
  treat its API as still-mobile and grep for new importers before changing a
  signature.
- **Sends** / **Receives**: none. `tapassets` is I/O-scoped to the tap-sdk
  wallet and its `Store`; it owns no actor and exchanges no actor messages.

## Invariants

- **Zero-fee node transactions.** `AssetTreeRequest.BatchOutput.Value` must
  equal the sum of the leaf `Amount`s exactly, and the leaf `AssetAmount`s must
  sum to `AssetAmount` exactly, which in turn must equal the sum of
  `TreeRootAssetSource.Inputs[i].Amount`. Tree transactions pay no fee, so any
  slack would be an unaccounted loss rather than a fee. All three sums are
  overflow-checked before use.
- **Every leaf carries assets and a unique cosigner.** A zero `AssetAmount`, a
  non-positive carrier value, a nil `CoSignerKey`, or a repeated cosigner key
  fails construction — a repeated cosigner would make the extracted client path
  ambiguous.
- **The batch output script is bound before materialization.**
  `buildAssetTree` requires `req.BatchOutput.PkScript` to byte-match
  `cfg.Root.BatchPkScript` and requires a non-zero `BatchOutpoint`, so a tree
  can never be materialized against an output the asset source does not
  describe. `Radix` must be at least 2.
- **The materialized tree is re-verified.** `buildAssetTree` calls
  `tree.Tree.Verify()` on the assembled result and fails if the tree is
  inconsistent, so a driver bug surfaces at construction rather than at signing
  time.
- **Commits are journaled, and the journal key is request-bound.** Each commit
  writes a `customAnchorCommitState` (version 0) holding the sealed package and
  a SHA-256 digest of the request under a domain-separated prefix. A replay
  with the same key but a *different* request digest is rejected rather than
  silently returning the old package — that mismatch means a key was reused,
  not that work is being retried. Asset tree commits key on
  `asset-tree/<Digest>/<input outpoint>`; batch anchor commits key on
  `asset-batch/<Digest>/<AssetRef>/<OutputIndex>`.
- **The journal write outlives request cancellation.**
  `storeStateAfterCommit` detaches with `context.WithoutCancel` and a 5-second
  timeout before persisting. The commit already happened in tapd; losing the
  record to a cancelled caller context would strand the transition with no way
  to replay it. See
  [`.claude/skills/context-lifecycle/SKILL.md`](../.claude/skills/context-lifecycle/SKILL.md).
- **Publication failure may be non-terminal.** `Publish` returns
  `ErrReconciliationRequired` when it cannot prove the publication did *not*
  land. Callers must check the outcome against tapd before retrying; a blind
  retry can double-spend the asset inputs.
- **Output indexes are final before deriving a request with change.** The
  change anchor's derivation commits to its `OutputIndex`, so reordering anchor
  transaction outputs after `DeriveScript` invalidates the derived script.

## Deep Docs

- [lib/tree/CLAUDE.md](../lib/tree/CLAUDE.md) — Tree layout, materialization,
  and `AssetTreeContext`.
- [lib/arkscript/CLAUDE.md](../lib/arkscript/CLAUDE.md) — Policy templates and
  taproot compilation.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
