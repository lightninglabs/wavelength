# tapassets

## Purpose

Adapts tap-sdk custom-anchor transactions to Wavelength's durable
boundaries, so a Taproot Asset can live inside an Ark VTXO. It is the
only package in the repo that talks to tapd: it onboards a confirmed
tapd asset into a composed boarding output, prepares out-of-round asset
transitions, assembles asset-aware VTXO trees, and claims an exited
asset leaf back into a tapd wallet. It owns no database, no RPC, and no
actor — every workflow is a synchronous, idempotent function driven by
`waved` and journaled through a `Store`.

## Composed Outputs

Every asset-bearing Ark output pays to

```
taproot(internal_key, tapBranch(sorted(policy_root, taproot_asset_root)))
```

The asset commitment root is a *sibling* of the ordinary Ark policy
root, so the semantic policy (collab leaf, exit leaf, CSV delay) is
unchanged and every script spend simply carries the asset root as one
extra top-level control-block sibling
(`arkscript.ComposeWithSiblingRoot`, `ComposeSpendInfoWithSibling`).
Byte equality between the recomputed composed script and the on-chain
one is what authenticates a disclosed asset root: finding a second
preimage set for the same output key would break taproot.

- `ComposedBoardingScript` (this package) delegates to
  `arkscript.ComposedBoardingScript` so the owner deriving a boarding
  address and the operator validating the boarding input run exactly one
  derivation. `arkscript.ComposedBoardingAddress` adds the address plus
  the tapscript whose collab inclusion proof gained the commitment leaf
  hash as its sibling.
- A VTXO descriptor stores the **composed** `PkScript`.
  `oortx.RecipientOutput.RoutingPkScript()` derives the *uncomposed*
  policy script, which is what owned-script routing and addressing use:
  composition only happens after the receiver has registered its
  semantic policy, so the receiver has to be addressable before the
  final script exists.

## Key Types

For field-level detail, use
`go doc github.com/lightninglabs/wavelength/tapassets.<Symbol>`.

- `Preparer` / `PreparerConfig` / `NewPreparer` (`preparer.go`,
  `partial_preparer.go`) — the durable, idempotent OOR asset preparer.
  Implements `oor.TaprootAssetOORPreparer` and
  `oor.TaprootAssetOORPreparationResumer`
  (`PrepareTaprootAssetOOR` / `ResumeTaprootAssetOOR`).
- `Onboarder` / `OnboarderConfig` / `OnboardingRequest` /
  `OnboardingResult` (`onboarding.go`) — journaled workflow moving a
  confirmed tapd asset anchor into a composed boarding output a round
  can consume.
- `ClaimRequest` / `ClaimResult` / `ClaimAssetVTXO` (`claim.go`) — the
  asset-aware final spend of a unilaterally exited asset leaf into a
  fresh tapd-owned anchor. Deliberately stateless.
- `Store` / `FileStore` / `NewFileStore` / `ErrStoreNotFound`
  (`store.go`) — the opaque state journal keyed by request ID.
- `CreatedAssetProofSource` / `ResolveCreatedAssetProofSource` /
  `CollectAssetProofPathAnchors` / `AssetProofPathAnchor`
  (`proof_source.go`) — the evidence model for an output this wallet
  created: a compact `tapsdk.AssetProofPath` plus the OP_TRUE witness.
- `ResolveOwnedAssetProofs` (`owned_proof.go`) — multi-UTXO selection of
  the daemon's own confirmed tapd proofs for one onboarding.
- `ExportBoardedProof` / `ErrBoardedProofPending` (`boarded_proof.go`) —
  exports the confirmed proof of a boarded output, which is absent from
  tapd's wallet inventory and therefore only findable through
  `ListTransfers`.
- `NewInventoryVerifier` / `NewBoardedProofVerifier`
  (`verifier_export.go`) — the two round-facing
  `tapsdk.ConfirmedProofVerifier` constructors.
- `BatchAnchorCommitter` / `BatchAnchorRequest` / `BatchAnchorCommit` /
  `DeriveBatchAnchorChange` (`batch_anchor.go`,
  `batch_anchor_publish.go`) — the round/batch side: commits an asset
  batch output on a caller-funded anchor, producing the compact-path
  root a VTXO tree chains beneath.
- `TreeMaterializer` / `TreeMaterializerConfig` / `BuildAssetTree` /
  `AssetTreeRequest` / `StandardVTXOLeafAnchor` (`tree_materializer.go`,
  `tree_assembler.go`, `tree_leaf_anchor.go`) — `tree.Materializer` for
  asset-aware VTXO trees: one asset transition per node, top-down.
- `ErrReconciliationRequired` — alias of
  `oor.ErrTaprootAssetCommitOutcomeUnknown`; returned when a tapd commit
  may or may not have landed and only a human or a resume can decide.

Unexported but load-bearing: `customAnchorDriver` / `sdkDriver`
(`driver.go`) is the *single* tap-sdk seam — no other file touches the
SDK builder; `preparationState` and `onboardingState` are the journals;
`assetSpendSource`, `assetTransitionVerifier`, `treePathVerifier`,
`proofInventoryVerifier`, `proofLineageVerifier` are the proof plumbing.

## Relationships

- **Depends on**: `lib/arkscript` (policy compilation, composed scripts,
  `ComposeWithSiblingRoot`), `lib/tree` (asset-aware tree
  construction + `AssetTreeContext`), `lib/tx/oor` (`RecipientOutput`,
  asset transfer container), `lib/tx/psbtutil` (anchor PSBT assembly),
  `oor` (the asset-transfer contract types it implements against),
  `vtxo` (`MaxTaprootAssetRefBytes` only), `tap-sdk` (+
  `taproot-assets/{asset,commitment,proof,tapscript}`),
  `lnd/keychain` (onboarding key descriptors).
- **Depended on by**: `waved` only — `taproot_assets_runtime.go`
  (constructs the store and preparer), `rpc_taproot_asset_onboarding.go`,
  `taproot_asset_board.go`, `taproot_asset_claim.go`,
  `rpc_taproot_asset_manage.go`, `server.go`.
- **Sends / Receives**: nothing. This package has no actor and sends no
  messages; it is called synchronously from `waved` RPC handlers and
  calls out to tapd over tap-sdk.

The dependency direction `tapassets → oor` (never the reverse) is the
whole point of `oor.TaprootAssetOORPreparer`: `oor` owns the declarative
contract and the carrier arithmetic, `tapassets` owns tapd.

## Onboarding

`Onboarder.Onboard` is idempotent on `OnboardingRequest.RequestID`, with
its journal under the `onboarding/` key prefix
(`onboardingStateVersion = 0`).

- `onboardingRequestDigest` hashes the request with proofs **sorted by
  content** (`sortedProofFiles`), so the digest is order-independent: a
  retry that enumerates the same UTXOs in a different order resolves the
  same journal instead of being rejected as a reused key.
- `ResolveOwnedAssetProofs` makes onboarding multi-UTXO: it selects
  unleased single-asset UTXOs by exact single match, then smallest
  sufficient, then accumulating smallest-first. Units therefore do not
  have to sit in one exactly-sized UTXO.
- `pinChange` returns asset change to the wallet on **pinned** keys
  (`DeriveScriptKey` + `DeriveInternalKey`, both persisted in the
  journal) and declares the change output in *External* script mode.
  tap-sdk's Wallet mode would derive fresh keys on every `Build`, and
  the split commitment binds the change script key, so a replay must
  rebuild the byte-identical transition.
- Conservation is **fail-closed**: `sum(inputs) == boarded + change`
  exactly, with every input carrying the same asset and the boarded
  output's script recomputed from the policy and the committed asset
  root before it is accepted. `checkOnboardingChange` re-derives the
  change anchor script from the pinned internal key.
- Carrier satoshis and the anchor miner fee come from the client's own
  lnd wallet (wallet-funded plan, `MaxFeeSat`, `CustomLockID`), not from
  the operator.
- The boarded output uses the **BOARDING** exit delay. Round admission
  rejects the shorter VTXO delay, so using it produces a boarding the
  operator will not take.
- `OnboardingResult.ScriptKey` is the only handle for exporting the
  boarded proof later, because the boarded output is deliberately absent
  from tapd's wallet inventory.

## OOR Preparation

One `PrepareTaprootAssetOOR` call produces **one** atomic transfer that
merges up to `oor.MaxTaprootAssetInputs = 8` asset VTXOs: N checkpoint
transitions feeding a single merged ark transition, never a sequence of
transfers.

- `preparationState` is version **3** and holds one sealed checkpoint
  package per asset ordinal (`CheckpointPackages`, indexed by the
  intent's spine-first input order) plus the Ark package, planned
  recipients, ordering nonce, and the carrier lease.
- Each input's OP_TRUE internal key is derived in its own domain:
  `checkpointAttempt(ordinal)` returns `"checkpoint/<ordinal>"`, used
  both as the journal attempt marker and as the deterministic key domain
  under `opTrueKeyDomain`. One function, so a marker and a key can never
  disagree about which input they belong to.
- Two digests, deliberately different in scope:
  - `preparationRequestDigest` (`"wavelength-asset-prepare-v3"`) pins
    the exact ordered outpoint set and everything else the package
    depends on. A changed request is a different package.
  - `preparationIntentDigest` (`"wavelength-asset-intent-v3"`) is
    **selection-independent**: it omits the inputs and the unit split so
    that a retry of a daemon-selected send resolves the *same* journal
    even though fresh selection would pick other coins. The receiver's
    effective units anchor the public intent instead.
- `assetSpendSource.validateTransitionCapacity` rejects a spend whose
  proof path cannot grow another step (`depth+2 >
  tapsdk.AssetProofPathMaxDepth`, and the step plus
  `assetProofPathPackageHeadroom` must fit under
  `tapsdk.AssetProofPathMaxSize`).
- `prepareMixedArk` converges the mixed transition to a canonical output
  order by retrying with an ordering nonce (`maxOrderingNonces = 256`,
  `maxOrderingIterations = 16`); the winning nonce is journaled so a
  resume rebuilds the same transaction.

## Proof Evidence

A created output's evidence is a compact `tapsdk.AssetProofPath`: one
verified confirmed base plus the unconfirmed transition steps above it.
`ResolveCreatedAssetProofSource` rebuilds it from a sealed package.

- The **spine** is the package input whose anchor outpoint equals the
  new step's `PreviousAnchorOutpoint` — tap-sdk emits the merged
  transition with `PrevOut` set to vPacket input 0. Every other asset
  input is a **co-input**.
- With more than one asset input the path is promoted to version
  **2**: the spine's path carries the new step, and each co-input's full
  compact path is attached as `CoInputPaths` on that step (a V2 path
  must leave `AdditionalBaseProofs` empty, so
  `promoteAssetProofPathV2` migrates them into `Steps[0].CoInputPaths`).
  Co-inputs are sorted stably by logical input index, so respend
  rebuilds the identical path.
- `CollectAssetProofPathAnchors` walks the spine and recurses through
  every co-input path, deduplicating by txid, so a claim knows the whole
  lineage DAG it must gather confirmations for.
- Version usage elsewhere: V0 is a plain confirmed-file base, V1 is the
  batch-anchor multi-source form (`AdditionalBaseProofs`).

## Proof Verification

`assetTransitionVerifier` (`verifier.go`) multiplexes one SDK verifier
callback across every asset input of a build. It keys on two different
things because the two questions are different:

- **Confirmed bases** route by `sha256` of the proof bytes
  (`confirmed map[[32]byte]tapsdk.ConfirmedProofVerifier`), registered
  from each source's proof file, every `AdditionalBaseProofs` entry, and
  recursively from all `CoInputPaths`. A miss fails closed. Chained
  verifiers are scoped to the proof file, never to a particular input,
  because two inputs may legitimately share a base.
- **Unconfirmed steps** route by the `(previous, anchor)` outpoint
  **edge** (`assetAnchorEdge`), consuming tap-sdk's
  `PreviousAnchorOutpoints`. Every outpoint is consumed at most once
  across the transfer DAG, so the edge is globally unique — unlike a
  step index, which repeats inside co-input paths. A miss returns nil
  (a historical step already bound when its own package committed); a
  hit requires exactly one previous outpoint matching the expectation
  and byte-identical anchor transaction bytes.

`treePathVerifier` (tree materialization) is the strict counterpart: it
binds **every** step by exact `StepIndex`, because a tree node's whole
path is known up front.

## Invariants

- Composed scripts are always recomputed and compared byte-for-byte
  before an output is accepted (onboarding boarded output and change,
  checkpoint outputs, planned recipients, claim output). Trusting a
  committed script instead would let tapd choose the Bitcoin topology.
- `driver.go` is the only file allowed to touch the tap-sdk builder. Every
  other file works against `commitResult` / `commitInput` /
  `commitOutput` / `commitProofSource`, so an SDK signature change lands
  in one place.
- **The journal is written before the tapd mutation, not after.**
  `Attempt` is persisted, then the commit runs. On failure,
  `commitOutcomeKnown(err)` decides: a known outcome clears the marker
  and returns the real error; an unknown one leaves the marker and
  returns `ErrReconciliationRequired`, because a cleared marker over a
  commit that actually landed would double-spend the asset.
- `FileStore` writes temp file → chmod 0600 → fsync file → rename →
  **fsync the containing directory**. Skipping the directory fsync can
  lose the commit-attempt marker while the tapd mutation survives, which
  is exactly the state the marker exists to prevent.
- Reusing a request ID with a different request digest is an error
  ("idempotency key reused with different request"), never a fresh
  workflow.
- An asset transfer crosses the point of no return at the **tapd
  commit**, earlier than an ordinary OOR transfer's server co-signing.
  This is why `oor.prePONRInputOutpoints` releases nothing once
  `TaprootAssetTransfer` is set.
- Onboarding requires an isolated asset per anchor
  (`len(anchor.Assets) == 1`); this is a prototype restriction, and
  loosening it means teaching the verifiers about passive assets rather
  than deleting the check.
- `ClaimAssetVTXO` is stateless and retried from scratch, so every step
  it performs must be idempotent against tapd
  (`DeclareScriptKey`, `ImportProofFile`).
- `ClaimRequest.SealedPackage` is **mandatory** — it is the leaf's only
  lineage, and the compact path plus the OP_TRUE witness are both
  recovered from it. This is the root of the known gap that an
  OOR-received asset VTXO cannot be claimed after a unilateral exit: the
  receiver's descriptor carries no sealed package.
- The claim's miner fee is paid out of the carrier
  (`outputValue = CarrierValueSat - FeeSat`), and the result must stay at
  or above `onboardingDustFloorSat = 330`.
- The generic unilateral-exit final sweep is deliberately **withheld**
  for asset targets (`unroll.behavior.resolveExitSpendPolicy` refuses
  when `desc.TaprootAssetRoot != nil`). A plain sweep would spend the
  carrier as satoshis and destroy the asset commitment; `ClaimAssetVTXO`
  is the asset-aware replacement.
- Asset amounts are asset **units**; Bitcoin carrier value is always a
  separate field. Nothing in this package converts between the two.
- Returned slices never alias `packageBytes` or tap-sdk memory.
- **Asset minting must use asset version 1.** An unconfirmed proof path
  needs a V1 base, and tapd's address default is V0, so addresses have
  to be created with `--asset_version 1`. This is an operational
  constraint on the tapd side; nothing in this package enforces it, and
  a V0 base fails later and less clearly.

## Deep Docs

- [docs/taproot_assets_architecture.md](../docs/taproot_assets_architecture.md)
  — Client-side Taproot Assets architecture: composed outputs,
  onboarding, boarding, OOR sends, carriers and reclaim, proof paths,
  exit and claim, and the known gaps.
- [oor/CLAUDE.md](../oor/CLAUDE.md) — The asset-transfer contract,
  input markers, and carrier allocation.
- [docs/arkscript_spec.md](../docs/arkscript_spec.md) — The tapscript
  policy system the asset root is composed beside.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
