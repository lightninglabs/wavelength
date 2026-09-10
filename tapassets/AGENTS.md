# tapassets

## Purpose

Builds asset boarding outputs (`Onboarder`), caller-funded batch anchors
(`BatchAnchorCommitter`), and asset VTXO trees (`BuildAssetTree`) through
tap-sdk. Durable journals retain committed packages for restart recovery.

## Key Types

- `Onboarder` — Funds and publishes a composed boarding output from tapd
  inventory, returns asset change, and journals the owner keys, sealed package,
  and signed transaction. `FileStore` supplies an atomic file-backed journal.
- `BatchAnchorCommitter` — Creates caller-funded asset batch outputs.
  `DeriveScript` produces the batch output script before funding, `Commit`
  seals the transfer package against a funded PSBT, and `Publish` records the
  finalized anchor PSBT in tapd.
- `BatchAnchorRequest` / `BatchAnchorCommit` / `BatchAnchorScript` — The
  request describing the asset move (sources, amount, cosigners, sweep leaf,
  output index), the sealed result, and the derived batch output script with
  its internal key, signing tweak, and Taproot Asset commitment root.
- `BatchAnchorSource` / `BatchAnchorChange` — One confirmed asset input
  (proof file, anchor outpoint, verifier), and the optional surplus output
  returned to the operator's tapd wallet via `DeriveBatchAnchorChange`.
- `BuildAssetTree` / `AssetTreeRequest` / `TreeMaterializerConfig` — Builds an
  asset VTXO tree beneath a batch output, returning a `lib/tree.Tree` whose
  node outputs commit to the asset transitions.
- `TreeRootAssetSource` / `TreeRootAssetInput` — The asset states held by the
  batch output that the tree's root node spends. `BatchAnchorCommit.RootSource`
  feeds straight into `TreeMaterializerConfig.Root`.
- `TreeLeafAnchor` / `StandardVTXOLeafAnchor` — Taproot material (uncomposed
  pkScript, internal key, canonical tap leaves) for a leaf VTXO output, before
  the asset root is composed in.
- `Store` — Journal persistence for completed transition packages. Implementations
  must replace each value atomically. `ErrStoreNotFound` signals an absent key.

## Relationships

- **Depends on**: `github.com/lightninglabs/tap-sdk` (custom-anchor requests,
  wallet, proof verifiers), `lib/tree` (`Node`, `Tree`, `LeafDescriptor`
  structures to materialize into), `lib/arkscript` (leaf policy construction
  for `StandardVTXOLeafAnchor`), `lib/tx/psbtutil` (PSBT encode/decode).
- **Depended on by**: no production caller yet; daemon onboarding and round
  runtime wiring consume these APIs in later integration work.
- **Sends** / **Receives**: none. This is a synchronous library, not an actor;
  it exchanges no mailbox messages.

## Invariants

- **Journaled commits are replay-safe, not retry-safe.** Every tapd commit goes
  through `customAnchorCommitJournal`: the sealed package is written to `Store`
  under a deterministic key before the caller sees it, and a later call with the
  same request digest decodes the journaled package instead of committing again.
  The journal write uses `context.WithoutCancel` plus a bounded timeout, so a
  cancelled request still records what tapd already sealed.
- **A journal key is bound to one request digest.** The digest is computed over
  the request under a domain-separated hash (`wavelength/asset-tree-request/v0`
  for trees). Loading state whose digest does not match the current request is
  an error, not a silent overwrite — it means the caller changed the request
  under a key that already sealed a different transition.
- **`ErrReconciliationRequired` means "do not retry blindly."** When tapd reports
  a publish attempt with an unknown outcome, `Publish` joins this sentinel onto
  the error. The transaction may already be in flight; the caller must check the
  real outcome before re-publishing.
- **Value and asset amount must both be conserved.** `AssetTreeRequest` requires
  `BatchOutput.Value` to equal the sum of leaf amounts (node transactions pay
  zero fee, for v3 ephemeral-anchor relay) *and* the leaf asset amounts to sum
  exactly to `AssetAmount`. Every leaf must carry a non-zero asset amount.
- **Batch anchor output indexes must be final before deriving change.**
  `BatchAnchorRequest.OutputIndex` and `BatchAnchorChange.OutputIndex` are
  committed to by the derived script, so the anchor transaction's output layout
  cannot shift after `DeriveScript`.
- **Exactly one of `ProofFile` and `ProofPath` is set** on a
  `TreeRootAssetInput`: a confirmed asset state versus one with unconfirmed
  transitions still in the path.
- An empty `Witness` on a source or root input asks tapd to sign that asset
  input; a non-empty stack is taken as caller-supplied authorization.

## Deep Docs

- [lib/tree/CLAUDE.md](../lib/tree/CLAUDE.md) — The tree structures this
  package materializes assets into.
- [lib/arkscript/CLAUDE.md](../lib/arkscript/CLAUDE.md) — Policy and taproot
  script construction used for leaf anchors.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
