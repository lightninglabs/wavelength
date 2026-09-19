# tapassets

## Purpose

Adapts tap-sdk custom-anchor transitions to Ark's VTXO tree shape. It moves
confirmed Taproot Assets into a single caller-funded *batch output*, then
materializes an asset-aware VTXO tree beneath that output so each tree leaf
carries both a Bitcoin carrier value and an asset amount. Every tap-sdk commit
runs through a durable journal so a restart replays the sealed package instead
of re-committing.

## Key Types

- `BatchAnchorCommitter` — Derives, seals, and publishes the caller-funded
  asset batch output. `DeriveScript` computes the batch output script *before*
  Bitcoin funding; `Commit` seals the transition against the funded PSBT;
  `Publish` verifies the finalized anchor PSBT and records it in tapd.
- `BatchAnchorRequest` / `BatchAnchorScript` / `BatchAnchorCommit` — Request,
  derived script material, and sealed result for one batch output. `Commit`
  carries the `TreeRootAssetSource` that feeds tree materialization.
- `BatchAnchorSource` / `BatchAnchorChange` — One confirmed asset input, and
  the optional surplus output returned to the operator's tapd wallet.
- `BuildAssetTree` / `AssetTreeRequest` / `TreeMaterializerConfig` — Builds an
  asset VTXO tree below a batch output, driving `lib/tree` materialization with
  a tap-sdk-backed node materializer.
- `TreeRootAssetSource` / `TreeRootAssetInput` — The asset states the root node
  spends, plus the batch output's taproot tweak and P2TR script.
- `TreeLeafAnchor` / `StandardVTXOLeafAnchor` — Taproot policy material for a
  leaf VTXO output, before the asset root is composed in.
- `Store` — Two-method journal (`Load`/`Store`) for sealed transition packages.
  Implementations must replace each value atomically and return
  `ErrStoreNotFound` for absent keys.

## Relationships

- **Depends on**: `lib/tree` (tree structure, `Materialize`, `LeafDescriptor`,
  `AssetContext`), `lib/arkscript` (unilateral-exit and sweep tapleaves),
  `lib/tx/psbtutil` (anchor PSBT serialization), and the external
  `github.com/lightninglabs/tap-sdk` wallet and custom-anchor builder.
- **Depended on by**: nothing yet. The package is a self-contained building
  block; the round/batch path does not import it. Wire-up will add
  `round`/`tapd` callers, at which point the Sends/Receives section below
  becomes relevant.
- **Sends**: no actor messages. This package is a synchronous library — it
  talks to tapd through the tap-sdk wallet, not through a mailbox.
- **Receives**: no actor messages.

## Invariants

- **Zero-fee node transactions.** The leaf carrier values must sum exactly to
  `BatchOutput.Value`, and the leaf asset amounts must sum exactly to
  `AssetAmount`; the root asset inputs must also sum to `AssetAmount`.
  `BuildAssetTree` rejects any mismatch rather than silently absorbing a
  difference as fee.
- **Output indexes are final before deriving change.** `BatchAnchorRequest`
  commits to `OutputIndex` (and the change output's index), so the anchor
  transaction's output ordering must be settled before `DeriveScript` runs.
- **A journal key is bound to one request.** The journal stores a SHA-256
  digest of the custom-anchor request under a domain tag
  (`wavelength/asset-batch-request/v0` for batch anchors,
  `wavelength/asset-tree-request/v0` for tree nodes). Loading a key whose
  stored digest differs from the current request is an error, not an
  overwrite — it means the key was reused for different content.
- **The journal write survives request cancellation.** After a successful tapd
  commit the sealed package is recorded under `context.WithoutCancel` with its
  own timeout, because dropping it would strand a committed transition that can
  no longer be replayed.
- **`ErrReconciliationRequired` means "do not blindly retry."** Publication may
  have already succeeded; the caller must check the outcome on chain or in tapd
  before issuing the request again.
- **Leaf cosigners are unique.** A repeated leaf cosigner key is rejected: the
  tree's per-leaf script derivation assumes distinct owners.
- **Asset amounts are non-zero.** Every leaf must carry a positive carrier
  value *and* a non-zero asset amount; a zero-asset leaf in an asset tree is a
  construction bug.

## Deep Docs

- [`lib/tree/CLAUDE.md`](../lib/tree/CLAUDE.md) — Tree structure,
  materialization contract, and `AssetContext`
- [`lib/arkscript/CLAUDE.md`](../lib/arkscript/CLAUDE.md) — Tapscript leaves
  used by the batch output and leaf anchors
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map
