# tapassets

## Purpose

Adapts tap-sdk custom-anchor asset transitions to Ark's transaction trees.
It does two jobs: move confirmed Taproot Asset units into a single
caller-funded *batch output* whose taproot key is a cosigner aggregate
(`BatchAnchorCommitter`), and then materialize an asset-carrying VTXO tree
beneath that batch output, committing one tap-sdk transition per tree node
(`BuildAssetTree`). Both paths are journaled so a restart replays the sealed
package instead of asking tapd to seal a second, conflicting transition.

## Key Types

For field-level detail, use
`go doc github.com/lightninglabs/wavelength/tapassets.<Symbol>`.

- `BatchAnchorCommitter` — Creates caller-funded asset batch outputs. Three
  ordered steps: `DeriveScript` (compute the batch output pkScript before
  Bitcoin funding), `Commit` (seal and validate the transition against the
  funded anchor PSBT), `Publish` (verify a finalized PSBT and record it in
  tapd). Built via `NewBatchAnchorCommitter(BatchAnchorCommitterConfig)`.
- `BatchAnchorRequest` / `BatchAnchorScript` / `BatchAnchorCommit` — Input,
  derived-script, and sealed-commit triple for the batch anchor flow.
  `BatchAnchorCommit.RootSource` is what feeds tree materialization.
- `BatchAnchorSource` — One confirmed asset input (proof file, amount,
  optional witness, anchor outpoint and internal key).
- `BatchAnchorChange` — Surplus assets returned to the operator's tapd
  wallet on its own anchor output; derived by `DeriveBatchAnchorChange`.
- `BuildAssetTree(ctx, TreeMaterializerConfig, AssetTreeRequest)` — Builds an
  asset VTXO tree below a batch output and returns a verified `tree.Tree`
  carrying a populated `tree.AssetTreeContext`.
- `TreeMaterializerConfig` — Wallet, `Store`, `AssetRef`, sweep leaf,
  `LeafAnchor` hook, root asset source, and the digest that scopes
  deterministic asset script keys.
- `TreeRootAssetSource` / `TreeRootAssetInput` — The asset states the root
  node spends, plus the batch output's signing tweak and pkScript.
- `TreeLeafAnchor` / `StandardVTXOLeafAnchor` — Taproot material for a leaf
  VTXO output: uncomposed policy pkScript, internal key, canonical tap
  leaves. `StandardVTXOLeafAnchor` builds it from an `arkscript` standard
  VTXO template.
- `Store` — Two-method journal interface (`Load`/`Store`) for sealed
  transition packages. `ErrStoreNotFound` signals an absent key.
- `ErrReconciliationRequired` — Returned (joined) by `Publish` when tapd
  reports an unknown publish outcome, so the caller must check whether the
  transfer landed before retrying.

## Relationships

- **Depends on**: `lib/tree` (structure building, `Materialize`,
  `AssetTreeContext`, `Tree.Verify`), `lib/arkscript` (VTXO policy templates
  and compiled taproot leaves for `StandardVTXOLeafAnchor`),
  `lib/tx/psbtutil` (PSBT encode/decode helpers), and the external
  `tap-sdk` (`tapsdk.Wallet`, custom-anchor request/commit types, proof
  verifiers).
- **Depended on by**: nothing in-repo yet. The package is a self-contained
  adapter layer; wiring it into a subsystem (round batch assembly, a daemon
  asset service) is future work, and the `Store` implementation will come
  from `db` when that happens.
- **Sends / Receives**: no actor messages. Every entry point is a synchronous
  call; the only cross-process interaction is with tapd via `tapsdk.Wallet`.

## Invariants

- **Value conservation is exact, both currencies.** `BuildAssetTree` rejects
  a request whose leaf carrier values do not sum to `BatchOutput.Value` and
  whose leaf asset amounts do not sum to `AssetAmount`; the root asset
  inputs must also carry exactly `AssetAmount`. Node transactions pay zero
  fee (v3 ephemeral-anchor relay), so any mismatch is a bug, not a fee.
- **Commits are journaled by request digest, not by attempt.**
  `customAnchorCommitJournal` keys on `asset-tree/{digest}/{outpoint}` and
  `asset-batch/{digest}/{assetRef}/{index}`, storing the sealed package
  under a SHA-256 digest of the domain-separated tap-sdk request. A replay
  with the same request decodes the journaled package; a replay of the same
  key with a *different* request is refused ("journal key reused with a
  different request") rather than silently resealing. `Store`
  implementations must replace each value atomically.
- **The journal write survives caller cancellation.**
  `storeStateAfterCommit` wraps the request context in
  `context.WithoutCancel` plus a 5s timeout: tapd has already mutated its
  state by then, so losing the journal record to a cancelled RPC would
  strand the transition.
- **`Commit` holds a mutex across the journal check and the tapd
  mutation.** Two concurrent commits for the same batch output would
  otherwise both miss the journal and seal conflicting transitions.
- **Output indexes must be final before deriving a request with change.**
  `BatchAnchorRequest.OutputIndex` and `BatchAnchorChange.OutputIndex` are
  baked into the derived scripts, so the anchor transaction's output layout
  cannot be reshuffled between `DeriveScript` and `Commit`. `Commit`
  re-checks that the funded transaction actually carries the derived batch
  script (and change script) at those indexes.
- **`ErrReconciliationRequired` is not a retry signal.** It means the
  publish outcome is unknown; retrying blindly risks a double publish. The
  caller must reconcile against chain/tapd state first.
- **Leaf policy is composed, not raw.** `TreeLeafAnchor.UncomposedPkScript`
  is the policy script *before* the asset commitment root is folded in; the
  materializer composes the final pkScript. Do not treat it as spendable.
- **Per-node taproot tweaks come from the asset context, not the sweep
  root.** The root node is tweaked by `TreeRootAssetSource.SigningTweak`
  (the batch output's tweak) and every child by the tweak its parent's
  commit handed off, each committing to that node's own asset commitment
  root. `MaterializeNode` records them via
  `AssetContext.SetSigningTweak(input, tweak)`, so signing must go through
  `Tree.NewTreeSignerSession` (which consults `AssetContext`) — a plain
  `tree.NewSignerSession` would apply the sweep tapscript root to every
  node and produce unusable signatures.
- **Asset script keys are deterministic and digest-scoped.**
  `TreeMaterializerConfig.Digest` / `BatchAnchorRequest.Digest` scope every
  derived key, so the same logical batch rebuilds byte-identically after a
  restart. Changing the digest changes every script.

## Deep Docs

- [lib/tree/CLAUDE.md](../lib/tree/CLAUDE.md) — Tree structure,
  materialization, and `AssetTreeContext`.
- [lib/arkscript/CLAUDE.md](../lib/arkscript/CLAUDE.md) — Policy templates
  used by `StandardVTXOLeafAnchor`.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
