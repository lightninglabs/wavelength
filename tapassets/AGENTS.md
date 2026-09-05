# tapassets

## Purpose

Adapts tap-sdk custom-anchor transitions to Ark trees, so a VTXO tree can carry
Taproot Assets alongside its Bitcoin value. Two entry points: a *batch anchor*
that moves confirmed assets into one caller-funded batch output, and a *tree
materializer* that walks that batch output down through branch nodes into asset
leaves, committing one tap-sdk transition per node transaction.

## Key Types

For field-level detail, use
`go doc github.com/lightninglabs/wavelength/tapassets.<Symbol>`.

- `BatchAnchorCommitter` — Creates caller-funded asset batch outputs. Three
  ordered steps: `DeriveScript` (compute the batch output script before
  Bitcoin funding), `Commit` (seal and validate the transition against the
  funded anchor PSBT), `Publish` (verify a finalized anchor PSBT and record it
  in tapd).
- `BatchAnchorRequest` / `BatchAnchorScript` / `BatchAnchorCommit` — Request,
  derived script material (`PkScript`, `InternalKey`, `SigningTweak`,
  `AssetRoot`), and sealed result (`PackageBytes`, `AnchorPSBT`, `RootSource`)
  for one batch anchor.
- `BatchAnchorSource` / `BatchAnchorChange` — One confirmed asset input, and
  the optional surplus output returned to the operator's tapd wallet
  (`DeriveBatchAnchorChange` derives its wallet keys).
- `BuildAssetTree` / `AssetTreeRequest` / `TreeMaterializerConfig` — Builds an
  asset VTXO tree below a batch output, committing each node's asset transition
  as it descends.
- `TreeRootAssetSource` / `TreeRootAssetInput` — The asset states the root node
  spends, plus the batch output's taproot tweak and P2TR script.
- `TreeLeafAnchor` / `StandardVTXOLeafAnchor` — Taproot data (uncomposed
  pkScript, internal key, canonical tap leaves) for a leaf VTXO output, before
  the asset root is folded in.
- `Store` — `Load`/`Store` key-value interface persisting sealed transition
  packages. Implementations must replace each value atomically.
- `ErrReconciliationRequired` — Publication may have succeeded; the caller must
  check its outcome before retrying.
- `ErrStoreNotFound` — A journal key has no durable value.

## Relationships

- **Depends on**: `lib/tree` (`Tree`, `Node`, `LeafDescriptor` — the tree the
  assets are materialized onto), `lib/arkscript` (leaf policy compilation and
  taproot spend info), `lib/tx/psbtutil` (PSBT encoding and signature
  attachment), `tap-sdk` (`Wallet`, `CustomAnchorTxBuilder`,
  `CustomAnchorTransferPackage` — the external Taproot Assets driver).
- **Depended on by**: nothing yet. The package is foundational: it is built and
  tested standalone ahead of the operator-side wiring that will call it.
- **Sends / Receives**: no actor messages. `tapassets` is a synchronous library
  called on the caller's goroutine, not an actor.

## Invariants

- Node transactions are **zero fee**. `AssetTreeRequest.BatchOutput.Value` must
  equal the sum of the leaf amounts exactly, and `AssetAmount` must equal the
  sum of the leaf asset amounts exactly. Every leaf must carry a non-zero asset
  amount.
- Every commit is **journaled before it is observable**. `commitDurably` keys a
  `customAnchorCommitState` by `asset-tree/<digest>/<outpoint>`, replays a
  previously sealed package on restart instead of re-committing, and refuses a
  key reused with a different request digest (the digest is a SHA-256 over the
  domain string plus the encoded request). The journal write runs under
  `context.WithoutCancel` with a 5s timeout, so a cancelled caller cannot lose
  a package tapd has already sealed.
- Commit state is versioned (`customAnchorCommitStateVersion = 0`) and bounded
  (`maxCustomAnchorCommitStateSize = 128 MiB`); an unsupported version or an
  out-of-range size is an error, never a silent reset.
- The driver commits with `SkipAnchorTxBroadcast` and `ExternalBroadcast` set:
  tapd seals and records the transition but **never broadcasts**. Broadcast is
  the caller's responsibility, via `Publish` after the anchor PSBT is finalized.
- A failure *after* tapd returned a committed response is distinct from a
  failure to commit: `commitResponseError` wraps the local conversion error, and
  `ErrReconciliationRequired` marks the publish path where the transaction may
  already be live. Neither may be treated as a clean retry.
- Batch output indexes must be final before deriving a request that carries
  change — `BatchAnchorRequest.OutputIndex` and `BatchAnchorChange.OutputIndex`
  are committed to by the derived script.
- The batch output's internal key is the untweaked cosigner aggregate; the
  `SigningTweak` commits to both the operator sweep leaf and the asset root, so
  the node keys are re-checked against the output being spent (`boundFinalKey`)
  before a MuSig2 signing plan is produced.
- Every tap-sdk output must carry a matching proof update; a missing proof blob
  fails the conversion rather than producing a tree node with no proof.

## Deep Docs

- [lib/tree/CLAUDE.md](../lib/tree/CLAUDE.md) — Tree construction this package
  materializes assets onto.
- [lib/arkscript/CLAUDE.md](../lib/arkscript/CLAUDE.md) — Policy compilation
  used for leaf anchors.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
