# tapassets

## Purpose

Builds asset boarding outputs (`Onboarder`), caller-funded batch anchors
(`BatchAnchorCommitter`), and asset VTXO trees (`BuildAssetTree`) through
tap-sdk, and verifies a client's asset VTXO before it signs (`VTXOVerifier`).
Durable journals retain committed packages for restart recovery.

## Key Types

- `Onboarder` — Moves tapd inventory into a composed boarding output that
  an `OnboardingFunder` pays for, returns asset change, and journals the owner
  keys, funded template, sealed package, and signed transaction. `FileStore`
  supplies an atomic file-backed journal.
- `OnboardingFunder` — The wallet that pays an onboarding's Bitcoin: fee
  estimates, funding an outputs-only template without signing it, leases, and
  signing its own inputs. tapd signs the asset anchors at publish, which
  `TapdSignsAnchorInputs` reports a tapd version can do.
- `Claimer` / `ClaimRequest` — Spends a confirmed composed output through
  its owner's exit leaf and moves the asset to keys the tapd wallet derives,
  so it becomes an ordinary holding. The claim keeps the output's full value
  and adds a zero-value P2A anchor, so the caller relays it with a CPFP
  child. The journal pins the derived keys before the commit and the signed
  transaction before the publish. `Prepare` signs a claim without recording
  it in tapd, so the caller can record it first. `TapdCanClaim` reports
  whether a tapd version credits a claim. A claimed output holds one input
  per issuance of its asset, and a claim always supplies their proof files
  with the publish, since tapd may never have seen their history. The
  journal's request digest leaves the proof files out, so a proof rebuilt
  after a reorganization still serves a journaled claim.
  `DecodeAssetVTXOClaimInputs` reads, for each issuance an asset VTXO's leaf
  holds, the proof path down to the leaf and the OP_TRUE witness from the
  leaf's sealed package.
- `SelectOnboardingProofs` / `ExportOnboardedProof` — Pick and export the
  confirmed proofs of the tapd anchors that fund an onboarding (one issuance,
  smallest covering anchor, else the largest anchors up to
  `MaxTransitionInputs`), and export the onboarding output's proof once its
  transfer confirms (`ErrOnboardedProofPending` before).
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
- `VTXOVerifier` — Implements the round package's `AssetVTXOVerifier`. It
  checks the sealed package of a client's asset leaf and verifies the leaf's
  proof path back to the confirmed deposits.
- `ProofChainVerifier` / `TapdProofChainVerifier` — The asset backend seam:
  chain verification of one confirmed proof file. tapd rejects a grouped
  asset whose group it does not know, and answers a valid proof of an asset
  it has not stored with an error, since it cannot describe the asset. The
  tapd implementation retries a rejected or failed file after inserting its
  issuance proof into tapd's universe. A reissuance carries no group key
  reveal, so tapd first syncs the group's anchor issuance from its federation
  servers.
- `LocalProofChainVerifier` — The asset backend seam for a backend that
  decides on its own chain view and trusted groups, such as tap-sdk's
  embedded engine. It asks the backend once and never writes to it.
- `Store` — Durable, atomic journal persistence for attempts and completed
  transition packages. `ErrStoreNotFound` signals an absent key. Callers own
  workflow serialization and wallet input reservations.

## Relationships

- **Depends on**: `github.com/lightninglabs/tap-sdk` (wallet) and its
  `customanchor` package (custom-anchor requests, proof verifiers),
  `lib/tree` (`Node`, `Tree`, `LeafDescriptor` structures to materialize
  into), `lib/arkscript` (leaf policy construction for
  `StandardVTXOLeafAnchor`), `lib/tx/psbtutil` (PSBT encode/decode).
- **Depended on by**: `waved`, which installs `VTXOVerifier` on the round
  actor when `assets.backend` is `tapd`, and funds asset deposits from its own
  wallet through `Onboarder` when tapd signs its own anchor inputs.
  Operator-side batch APIs have no production caller in this repository.
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
  real outcome before re-publishing. A claim is the exception: once tapd
  reports no record of it, `Claimer` publishes it again, because tapd keys a
  transfer by its anchor transaction and a second publish attaches to the
  first.
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
- **The funder pays, and tapd only commits.** The funder sees an outputs-only
  template whose asset outputs are reduced, down to dust, by the spent
  anchors' value net of the fee for their key-path weight, so it selects only
  what the anchors leave uncovered; anchor value beyond that goes to its
  change. The Onboarder then lays the transaction out as anchors, funder
  inputs, asset outputs, funder change, and commits it caller-funded exact,
  so tapd adds no input or output. The funder may reorder what it funds; only
  its inputs and change are taken from its template. Each funder input must
  spend a native SegWit output (`NativeSegWitScript`), since a scriptSig
  would change the txid tapd seals. Fee rates stay in sat/kWU throughout.
- **Funded coins are journaled before the commit.** A retry reuses the
  journaled template and renews its leases, so it spends the same coins, and
  a sealed package renews them until it publishes. A refused commit, or a fee
  above `MaxFeeSat`, releases them and clears the template, unless the
  release fails, which keeps the template for the retry. Releases and the
  template's journal write outlive a cancelled caller; an ambiguous commit
  keeps the coins.
- **tapd signs its anchors at publish.** Every anchor has a backend-signed
  plan, so it reaches publication unsigned, and the funder signs and finalizes
  only its own inputs. The funder signs a copy in which the anchors carry no
  derivation info and its inputs carry the derivations journaled with the
  funded template, so a wallet that shares tapd's keys cannot sign an anchor.
  The Onboarder rejects a package that is not caller-funded exact.
- **Anchor leaves must assemble into their Ark policy tree.** tap-sdk pairs
  adjacent `TapLeaves`, while arkscript splits a policy's leaves in halves.
  `anchorPlan` rejects leaves whose two roots differ (any leaf count that is
  not a power of two, or leaves out of canonical order), since the composed
  output would not match the policy's control blocks.
- **A transition merges at most `MaxTransitionInputs` inputs per issuance.**
  Each extra input becomes a co-input path of the proof path step, which
  tap-sdk bounds. Plans above the bound fail with
  `ErrTooManyTransitionInputs` before tapd seals anything.
- **An asset VTXO is verified against its round, not a wallet.**
  `VTXOVerifier` requires the leaf package to seal the leaf's own
  transaction, and every asset output to land in the VTXO under an OP_TRUE
  script key and commit to the leaf's asset root. Every unconfirmed proof
  step must anchor in the commitment transaction or on the client's tree
  path. Deposits from other clients are not in this wallet, so their anchor
  inventory comes from `customanchor.DeriveConfirmedProofInventory`, and the
  backend only verifies the confirmed proof chain.

## Deep Docs

- [lib/tree/CLAUDE.md](../lib/tree/CLAUDE.md) — The tree structures this
  package materializes assets into.
- [lib/arkscript/CLAUDE.md](../lib/arkscript/CLAUDE.md) — Policy and taproot
  script construction used for leaf anchors.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
