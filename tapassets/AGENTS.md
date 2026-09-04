# tapassets

## Purpose

Adapts the external tap-sdk custom-anchor transition API to Ark's batch output
and VTXO tree shapes. It moves confirmed Taproot Asset states into a single
caller-funded batch output, then materializes an asset-carrying VTXO tree
beneath that output so every tree transaction also commits one Taproot Asset
transition.

## Key Types

- `BatchAnchorCommitter` — Serialized, journal-backed committer for one asset
  batch output. `DeriveScript` computes the output script *before* Bitcoin
  funding, `Commit` seals the asset transition against the funded anchor PSBT,
  and `Publish` records the finalized transaction in tapd.
- `BatchAnchorRequest` / `BatchAnchorSource` / `BatchAnchorChange` — The
  requested asset amount, the confirmed asset inputs that fund it, and the
  optional surplus output returned to the operator's tapd wallet.
  `DeriveBatchAnchorChange` derives the change output's wallet keys.
- `BatchAnchorScript` — Pre-funding derivation of the batch output: composed
  `PkScript`, untweaked cosigner `InternalKey`, `SigningTweak`, `AssetRoot`,
  and `ChangePkScript`.
- `BatchAnchorCommit` — Sealed transfer package, tapd-signed anchor PSBT, the
  echoed script derivation, and the `TreeRootAssetSource` that feeds tree
  materialization.
- `BuildAssetTree` / `AssetTreeRequest` / `TreeMaterializerConfig` — Builds an
  asset VTXO tree below a batch output, committing one custom-anchor transition
  per node and returning a `lib/tree.Tree` with a populated `AssetContext`.
- `TreeRootAssetSource` / `TreeRootAssetInput` — The asset states held by the
  batch output that the tree root spends, plus that output's taproot tweak and
  P2TR script.
- `TreeLeafAnchor` / `StandardVTXOLeafAnchor` — A leaf's policy taproot
  material (uncomposed pkScript, internal key, canonical tap leaves), which the
  materializer composes with the leaf's asset commitment root.
- `Store` / `ErrStoreNotFound` — Durable key/value journal for sealed
  transition packages. Implementations must replace each value atomically.
- `ErrReconciliationRequired` — `Publish` could not determine whether tapd
  accepted the transfer; the outcome must be checked before any retry.

## Relationships

- **Depends on**: `lib/tree` (`Node`, `Tree`, `AssetTreeContext`,
  `BuildStructure`/`Materialize`, `ComputeInternalKey`, per-node signing
  tweaks), `lib/arkscript` (leaf policy compilation and taproot spend info),
  `lib/tx/psbtutil` (anchor PSBT parse/encode), and the external
  `github.com/lightninglabs/tap-sdk` custom-anchor API.
- **Depended on by**: nothing yet. The package is a self-contained adapter and
  is not wired into `waved`, `round`, or any actor. Treat the exported surface
  as the intended integration point, not a live dependency.
- **Sends**: no actor messages. Every call is a direct, synchronous SDK call.
- **Receives**: no actor messages.

## Invariants

- **Derive before funding, then re-derive after committing.** `DeriveScript`
  produces the batch output script before the Bitcoin transaction is funded.
  `Commit` fails if the committed taproot merkle root, asset root, or composed
  pkScript diverges from that pre-funding derivation. This is what makes it
  safe to fund and sign Bitcoin before the asset commitment exists.
- **Commits are journaled by request digest.** `commitDurably` keys each tapd
  mutation under a domain-separated SHA-256 digest of the JSON-encoded
  `CustomAnchorRequest` (`wavelength/asset-batch-request/v0` for batch anchors,
  `wavelength/asset-tree-request/v0` for tree nodes). A replay decodes the
  stored sealed package instead of re-committing. Reusing a journal key with a
  *different* request is refused rather than overwritten.
- **The journal write survives caller cancellation.** After tapd accepts a
  commit, the package is stored on a context detached with
  `context.WithoutCancel` under a 5-second timeout, so a cancelled caller
  cannot lose a completed tapd mutation.
- `BatchAnchorCommitter.Commit` holds its mutex across the journal read and the
  tapd mutation, keeping the check and the mutation in one critical section.
- **Ark broadcasts, tapd only records.** Commits set
  `SkipAnchorTxBroadcast`/`ExternalBroadcast`, so the SDK never broadcasts the
  anchor transaction. `Publish` verifies the finalized PSBT and records it.
- `Publish` joins `ErrReconciliationRequired` when the SDK reports
  `OutcomeUnknown`: publication may have succeeded, so a blind retry is unsafe.
- Batch anchors must commit in `CustomAnchorFundingCallerFundedExact` mode with
  `CustomAssetScriptOPTrue` outputs, and the committed anchor PSBT's txid must
  equal the funded transaction's txid.
- **Output indexes must be final before deriving a request with change.** The
  change output's derivation binds to its position in the anchor transaction.
- **Tree transactions are zero fee.** The batch output value must equal the sum
  of the leaf carrier amounts; leaf asset amounts must sum to
  `AssetTreeRequest.AssetAmount`, which must in turn equal the sum of
  `TreeMaterializerConfig.Root.Inputs` amounts. Every leaf needs a non-zero
  asset amount and a cosigner that appears exactly once; `Radix` must be >= 2.
- The batch output must be P2TR, the root signing tweak exactly 32 bytes, and
  `Digest` non-zero — `Digest` also domain-scopes the deterministic OP_TRUE
  asset script keys, so distinct trees never reuse a script key.
- **Every node's key is bound to the output it spends.** `boundFinalKey`
  recomputes the cosigner aggregate, applies the parent's asset-committing
  tweak, and fails unless the result reproduces the spent pkScript byte for
  byte. A node is never signed against a key that does not open its parent.
- Per-node commits validate that inputs match their requested sources one to
  one, that outputs conserve each issuance's amount exactly
  (`validateIssuanceConservation`), and that the committed transaction is
  byte-identical to the node template.
- **Proof capacity is checked before materialization.**
  `validateTreeProofCapacity` rejects a tree whose depth would push any root
  input's proof past `AssetProofPathMaxDepth` or `AssetProofPathMaxSize`.
- `treePathVerifier` splits verification by depth: steps below the input's base
  depth delegate to that input's own verifier, while appended tree steps must
  match the expected previous outpoint, anchor outpoint, anchor transaction
  bytes, and input outpoint set exactly.
- `MuSig2` cosigner slices are copied before aggregation and before signing;
  `musig2.AggregateKeys` and local signers sort their input in place.

## Deep Docs

- [lib/tree/CLAUDE.md](../lib/tree/CLAUDE.md) — Tree structure, materialization
  interface, and `AssetTreeContext`.
- [lib/arkscript/CLAUDE.md](../lib/arkscript/CLAUDE.md) — Policy compilation
  and taproot spend info.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
