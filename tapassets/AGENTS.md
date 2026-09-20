# tapassets

## Purpose

Adapts tap-sdk custom-anchor transitions to Ark trees. Two related jobs: move
confirmed Taproot Assets into a single caller-funded *batch output*
(`BatchAnchorCommitter`), and materialize an asset-aware VTXO tree beneath that
batch output (`BuildAssetTree`), committing one tap-sdk transition per tree
node. Every tapd mutation is journaled first, so a crash between "tapd sealed
the transition" and "we recorded it" replays instead of double-spending the
asset.

## Key Types

- `BatchAnchorCommitter` / `BatchAnchorCommitterConfig` — Creates caller-funded
  asset batch outputs. Three-step protocol: `DeriveScript` (compute the batch
  output script *before* Bitcoin funding) → caller funds and signs the anchor
  PSBT → `Commit` (seal and validate the transition against the funded tx) →
  `Publish` (verify the finalized PSBT and record it in tapd).
- `BatchAnchorRequest` — One caller-funded move of confirmed assets: asset ref,
  amount, funding `Sources`, optional `Change`, cosigner set, operator
  `SweepLeaf`, script-scoping `Digest`, and the batch output's index/value.
- `BatchAnchorScript` — Derived batch output: `PkScript`, untweaked cosigner
  `InternalKey`, `SigningTweak` (commits to sweep leaf + asset root),
  `AssetRoot`, and `ChangePkScript` when the request carries change.
- `BatchAnchorCommit` — The sealed transition plus the `TreeRootAssetSource`
  that tree materialization consumes.
- `BatchAnchorSource` / `BatchAnchorChange` — One confirmed asset input (proof
  file, amount, verifier, anchor outpoint/internal key) and the surplus
  returned to the operator's tapd wallet; `DeriveBatchAnchorChange` derives the
  change keys.
- `BuildAssetTree(ctx, TreeMaterializerConfig, AssetTreeRequest)` — Builds the
  asset VTXO tree below a batch output and returns a `lib/tree.Tree`.
- `TreeMaterializerConfig` — tap-sdk wallet, durable `Store`, asset ref, sweep
  leaf, per-leaf `LeafAnchor` callback, root asset source, and the script-key
  `Digest`.
- `AssetTreeRequest` — Leaves, operator key, radix, batch outpoint/output, and
  the total `AssetAmount` the leaves must sum to.
- `TreeRootAssetSource` / `TreeRootAssetInput` — The asset states the tree root
  spends, plus the batch output's taproot tweak and P2TR script.
- `TreeLeafAnchor` / `StandardVTXOLeafAnchor` — Taproot data (uncomposed
  pkScript, internal key, canonical tap leaves) for a leaf VTXO output; the
  standard helper builds the owner/operator VTXO policy with an exit delay.
- `Store` — Durable journal for completed transition packages: `Load`/`Store`
  keyed by string. `ErrStoreNotFound` reports a missing key.
- `ErrReconciliationRequired` — Publication may have succeeded; the outcome
  must be checked before retrying.

## Relationships

- **Depends on**: `lib/tree` (`Tree`, `Node`, `LeafDescriptor` — the tree the
  materializer fills in), `lib/arkscript` (VTXO policy leaves and taproot
  script composition), `lib/tx/psbtutil` (anchor PSBT encoding),
  `github.com/lightninglabs/tap-sdk` (custom-anchor builder, wallet, proof
  verifier), `btcd` (`btcec`, `txscript`, `wire`, `psbt`).
- **Depended on by**: nothing yet — the package is self-contained and not
  wired into the daemon. The `round` asset-request and `db` asset-state
  plumbing that will drive it land separately.
- **Sends** / **Receives**: none. `tapassets` is a synchronous library, not an
  actor; it exchanges no `lib/actormsg` messages and registers no service key.

## Invariants

- **Node transactions are zero fee.** `AssetTreeRequest.BatchOutput.Value` must
  equal the sum of the leaf amounts, and the leaf asset amounts must sum
  exactly to `AssetAmount`. Every leaf must carry a non-zero asset amount.
- **Every tapd mutation is journaled before it is reported complete.**
  `customAnchorCommitJournal.commitDurably` loads the journal key first and
  replays the sealed package when one is present, so a retry after a crash
  decodes the existing transition instead of asking tapd to commit again. Batch
  anchors key on `asset-batch/<digest>/<asset-ref>/<output-index>` under domain
  `wavelength/asset-batch-request/v0`; tree nodes key on
  `asset-tree/<digest>/<input>` under `wavelength/asset-tree-request/v0`.
- **A journal key reused with a different request is a hard error.** The stored
  state carries a domain-separated SHA-256 of the request; a digest mismatch
  fails the commit rather than overwriting a sealed package.
- **The journal write outlives request cancellation.**
  `storeStateAfterCommit` runs under `context.WithoutCancel` with a 5s timeout,
  because tapd has already mutated its state by then — dropping the record to a
  cancelled RPC context would strand the transition.
- **Anchor broadcast is external.** `Commit` sets `SkipAnchorTxBroadcast` and
  `ExternalBroadcast`, so the caller broadcasts the anchor transaction itself
  and only then calls `Publish` with the final PSBT.
- **`ErrReconciliationRequired` is not a retry signal.** It is returned when
  tap-sdk reports `OutcomeUnknown` on a publish attempt; the transfer may be
  live on chain, so the caller must reconcile before re-publishing.
- `BatchAnchorCommitter.Commit` holds a mutex across the journal check and the
  tapd mutation, keeping the two in one critical section.
- Output indexes must be final before deriving a request with change: `Commit`
  re-checks that the funded transaction's output at `req.OutputIndex` carries
  the derived batch script, and validates the change output the same way.
- Exactly one of `TreeRootAssetInput.ProofFile` and `ProofPath` may be set — a
  confirmed proof or an unconfirmed transition path, never both.
- `Store` implementations must replace each value atomically.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
- [lib/tree/CLAUDE.md](../lib/tree/CLAUDE.md) — The tree model this package
  materializes asset commitments into.
