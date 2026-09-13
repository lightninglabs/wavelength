# tapassets

## Purpose

Adapts tap-sdk custom-anchor asset transitions to Ark's output shapes. Two
things live here: moving confirmed Taproot Assets into a single caller-funded
**batch output** (the anchor a round commits to), and **materializing** the
VTXO tree beneath that batch output so every node and leaf carries the asset
commitment alongside its ordinary Ark policy script.

The package is a pure adapter over `tapsdk.Wallet`: it owns no actor, no
chain access, and no scheduling. Callers fund and broadcast the Bitcoin
anchor transaction themselves; `tapassets` only derives scripts, seals asset
transitions, and hands back a `lib/tree.Tree` whose outputs already commit to
the right asset roots.

Not yet wired into the daemon — no repo package imports it. Treat the seams
below (`Store`, `TreeMaterializerConfig.LeafAnchor`, the funding handshake)
as the integration contract when that wiring lands.

## Key Types

- `BatchAnchorCommitter` — Three-step funding handshake for the batch output:
  `DeriveScript` composes the batch (and optional change) pkScript from an
  unfunded PSBT template, the caller funds and finalizes the Bitcoin side,
  `Commit` seals and validates the asset transition against the funded packet,
  and `Publish` records the finalized anchor PSBT in tapd.
- `BatchAnchorRequest` / `BatchAnchorSource` / `BatchAnchorChange` — The
  request shape: confirmed asset inputs, the cosigner set aggregated into the
  batch internal key, the operator sweep leaf, and the optional surplus that
  returns to the operator's tapd wallet on its own anchor output.
- `BatchAnchorScript` — Derivation result: composed `PkScript`, untweaked
  `InternalKey`, the `SigningTweak` committing to both sweep leaf and asset
  root, and `AssetRoot`.
- `BatchAnchorCommit` — Sealed transfer package plus the `TreeRootAssetSource`
  that feeds tree materialization.
- `BuildAssetTree` / `AssetTreeRequest` / `TreeMaterializerConfig` — Builds an
  asset-aware VTXO tree below a batch output, returning a `lib/tree.Tree`.
  `LeafAnchor` is the injection point that turns a tree node into leaf policy
  material; `StandardVTXOLeafAnchor` supplies the ordinary
  owner/operator-plus-CSV VTXO policy.
- `TreeLeafAnchor` — Taproot data for one leaf VTXO output: the policy script
  *before* the asset root is composed in, the internal key, and the canonical
  tap leaves.
- `TreeRootAssetSource` / `TreeRootAssetInput` — The asset states the tree root
  spends out of the batch output, with the batch tweak and pkScript.
- `Store` — Two-method (`Load`/`Store`) durable journal the package uses to
  replay sealed commits after a restart. Implementations must replace each
  value atomically.
- `ErrReconciliationRequired` — Sentinel joined onto a `Publish` failure whose
  outcome is unknown. See the invariant below.
- `ErrStoreNotFound` — What a `Store` must return for an absent key; any other
  error is treated as a real load failure.

## Relationships

- **Depends on**: `lib/tree` (`Tree`, `Node`, `LeafDescriptor` — the tree the
  materializer builds and annotates), `lib/arkscript` (policy templates and
  taproot composition for batch, sweep, and leaf scripts), `lib/tx/psbtutil`
  (PSBT encode/decode and signature attachment on node transactions), and the
  external `github.com/lightninglabs/tap-sdk` (`Wallet`, `CustomAnchorRequest`,
  `ConfirmedProofVerifier`, asset refs and hashes).
- **Depended on by**: nothing yet. The intended consumers are `round` (asset
  round tree construction) and `db` (as the `Store` implementation).
- **Sends** / **Receives**: none — the package exposes plain function and
  method calls, not actor messages.

## Invariants

- **Node transactions are zero fee.** `AssetTreeRequest.BatchOutput.Value` must
  equal the sum of the leaf amounts exactly, and `AssetAmount` must equal the
  sum of the leaves' asset amounts. Every leaf must carry a non-zero asset
  amount.
- **Output indexes must be final before deriving a request with change.**
  `DeriveScript` commits to `OutputIndex` (and the change output's index), so
  the caller cannot reorder the anchor transaction's outputs afterwards.
- **`ErrReconciliationRequired` means "do not blindly retry".** It is joined
  onto a `Publish` error only when tapd reports
  `CustomAnchorPublishAttemptError.OutcomeUnknown` — publication may already
  have succeeded. The caller must check the transfer's real outcome before
  attempting the transition again; a naive retry risks double-spending the
  asset inputs.
- **The commit journal is keyed by a request digest, not just by outpoint.**
  Each sealed package is stored under `asset-tree/<digest>/<outpoint>` together
  with a hash of the originating `CustomAnchorRequest` (domain-separated by
  `wavelength/asset-tree-request/v0`). Loading a key whose stored digest does
  not match the current request is a hard error — reusing a journal key for a
  different request is a bug, not something to paper over.
- **Journal writes survive request cancellation.** `storeStateAfterCommit`
  detaches from the caller's context (`context.WithoutCancel`) under a 5s
  timeout, because the asset transition is already sealed in tapd by then:
  dropping the write would strand a commit the journal can no longer replay.
- **Replay returns the journaled package, never a fresh commit.** When a stored
  package exists for the key, `commitDurably` decodes and returns it instead of
  re-sealing, so a restart mid-flight reproduces byte-identical asset state.
- **An empty `Witness` stack means "tapd signs".** On both `BatchAnchorSource`
  and `TreeRootAssetInput`, a populated witness is a caller-supplied
  authorization and is used verbatim.
- `TreeRootAssetInput` must set exactly one of `ProofFile` (confirmed state) and
  `ProofPath` (state with unconfirmed transitions).
- `TreeLeafAnchor.UncomposedPkScript` is deliberately the *pre-asset* script:
  the materializer composes the asset root into it. Passing an
  already-composed script double-commits and yields an unspendable leaf.

## Deep Docs

- [lib/tree/CLAUDE.md](../lib/tree/CLAUDE.md) — Tree construction the
  materializer builds on.
- [lib/arkscript/CLAUDE.md](../lib/arkscript/CLAUDE.md) — Policy templates and
  taproot composition.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
