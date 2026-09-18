# tapassets

## Purpose

Adapts tap-sdk custom-anchor transitions to Ark trees. The package turns
confirmed Taproot Asset inputs into a caller-funded *batch output*, then
materializes an asset-aware VTXO tree beneath that batch output so every node
transaction carries a valid asset commitment alongside its Bitcoin script.

It is a pure adapter layer: it owns no actors, no database tables, and no RPC
surface. Callers supply a `tapsdk.Wallet` for signing/sealing and a `Store`
for replay-safe journalling, and receive sealed transfer packages plus a
`lib/tree.Tree` in return.

## Key Types

- `BatchAnchorCommitter` — creates caller-funded asset batch outputs. Drives
  the three-step `DeriveScript` → `Commit` → `Publish` flow so the batch
  output's script is known before Bitcoin funding, sealed after funding, and
  recorded in tapd only once the anchor PSBT is final.
- `BatchAnchorRequest` / `BatchAnchorScript` / `BatchAnchorCommit` — the
  input, derived script material, and sealed result of one batch anchor.
- `BatchAnchorSource` — one confirmed asset input (proof file, amount,
  witness, anchor outpoint and internal key).
- `BatchAnchorChange` — surplus assets returned to the operator's tapd wallet
  on its own anchor output. Built with `DeriveBatchAnchorChange`.
- `BuildAssetTree` / `TreeMaterializerConfig` / `AssetTreeRequest` — builds
  the asset VTXO tree below a batch output and returns a `lib/tree.Tree`.
- `TreeRootAssetSource` / `TreeRootAssetInput` — the asset states spent by the
  tree's root node, plus the batch output's taproot tweak and script.
- `TreeLeafAnchor` — taproot data (uncomposed script, internal key, canonical
  tap leaves) for a leaf VTXO output. `StandardVTXOLeafAnchor` builds the
  standard owner/operator/exit-delay leaf.
- `Store` — narrow two-method journal (`Load`/`Store`) persisting completed
  transition packages. Implementations must replace each value atomically.

## Relationships

- **Depends on**: `lib/tree` (node/leaf descriptors and the returned `Tree`),
  `lib/arkscript` (VTXO and sweep policy script templates), `lib/tx/psbtutil`
  (anchor PSBT manipulation), and the external `tap-sdk` (wallet, custom
  anchor requests, proof verification).
- **Depended on by**: nothing in-repo yet. The package is staged ahead of the
  round-side wiring that will consume it; asset round state currently lives in
  `round` (`asset_request.go`, `asset_vtxo_state.go`) and `db`
  (`vtxo_asset_state.go`, `tree_codec`).
- **Sends**: no actor messages. Synchronous function and method calls only.
- **Receives**: no actor messages.

## Invariants

- `AssetTreeRequest.BatchOutput` value must equal the sum of the leaf
  amounts — node transactions are zero fee — and the leaf asset amounts must
  sum to `AssetAmount` exactly. Every leaf must carry a non-zero asset amount.
- `BatchAnchorRequest` output indexes must be final before deriving a request
  that carries change: the change anchor script commits to its output
  position.
- The funding sources must carry exactly `BatchAnchorRequest.Amount` plus any
  change.
- `DeriveScript` must run before Bitcoin funding and `Commit` after it. The
  batch output script (and therefore the funded PSBT) depends on the derived
  asset root, so reordering the two produces a package tapd will reject.
- `ErrReconciliationRequired` means publication may have succeeded. Callers
  must check the outcome on chain before retrying rather than re-publishing:
  a blind retry can double-spend the asset inputs.
- `ErrStoreNotFound` means a journal key has no durable value; it is the
  expected first-attempt result, not a failure.
- The commit journal keys on `asset-tree/{Digest}/{input outpoint}` and stores
  a request digest alongside the sealed package. A cached package is only
  replayed when the recomputed digest matches, so a changed request
  re-commits instead of silently reusing a stale transition.
- `Store` implementations must replace each value atomically: a torn write
  leaves the journal unable to distinguish "not yet committed" from
  "committed but unreadable".

## Deep Docs

- [lib/tree/CLAUDE.md](../lib/tree/CLAUDE.md) — VTXO tree construction.
- [lib/arkscript/CLAUDE.md](../lib/arkscript/CLAUDE.md) — Script templates.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
