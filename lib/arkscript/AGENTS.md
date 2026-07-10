# lib/arkscript

## Purpose

Bitcoin script compiler and policy system for constructing Ark protocol taproot
outputs. Provides an AST of semantic nodes that compile to tapscript, plus
higher-level policy templates for VTXO, vHTLC, and checkpoint outputs with
validated invariants.

## Key Types

- `Node` — Sealed interface for all AST nodes representing spending conditions.
  Implementations: `Multisig`, `CSV`, `Condition`, `Preimage`, `CLTV`.
- `PolicyTemplate` — Semantic representation of a tapscript policy with named
  leaves. Supports encode/decode for persistence.
  - `PolicyTemplate.MatchesPkScript(pkScript []byte) bool` — Compiles the
    policy and checks whether it produces the given P2TR script. Used by
    `BuildCustomTransferInputs` to bind a caller-supplied policy to the
    on-chain output before signatures are produced.
- `CompiledPolicy` — Fully compiled policy with canonical leaf ordering, merkle
  tree, and control block derivation.
- `VTXOPolicy` — Compiled VTXO taproot policy with collab and exit spend paths.
  Provides `CollabSpendInfo()` and `ExitSpendInfo()`.
- `VHTLCPolicy` — 6-leaf vHTLC policy with claim/refund/unilateral paths for
  hash-time-locked conditional transfers.
- `CheckpointPolicy` — Parameters for OOR checkpoint taproot tree construction. `CheckpointTapScript` / `CheckpointPkScript` derive the checkpoint output.
- `SpendInfo` — Witness script + control block needed to spend a specific leaf. Methods: `BuildSignDescriptor`, `CollabWitness`, `TimeoutWitness`.
- `SpendPath` — Serializable spend path (leaf index + encoded leaf data) with `Witness` and `AttachTapLeafScript` helpers for PSBT integration.
  - `SpendPath.VerifyBindsToPkScript(pkScript []byte) error` — Checks that the
    spend path's witness script and control block commit to a taproot output
    whose P2TR script exactly matches `pkScript`. Prevents a caller from
    supplying a control block for an unrelated tap tree and obtaining a
    signature over an attacker-chosen tapscript.
- `StandardVTXOParams` — Decoded parameters for a standard Ark VTXO policy (owner key, operator key, CSV delay). Derived via `DecodeStandardVTXOParams`.
- `ComposedPolicy` — Composes an existing `CompiledPolicy` with an additional sibling tap branch root, allowing sub-tree aggregation. Built with `ComposeWithSiblingRoot`.
- `AnchorOutput` / `AnchorPkScript` — `AnchorPkScript` is the standard P2A keyless pkScript; `AnchorOutput` returns a zero-value `wire.TxOut` carrying it for CPFP fee bumping.
- `EncodedLeaf` / `EncodeTapTree` / `DecodeTapTree` — TLV-based tap tree serialization for PSBT sidecars compatible with `waddrmgr` format.
- `EncodeStandardVTXOArtifacts(ownerKey, operatorKey, exitDelay)` — Convenience
  helper returning both the encoded policy template bytes and the canonical P2TR
  pkScript for the standard VTXO shape. Use `tree.NewVTXODescriptor` when the
  full tree-construction descriptor is also needed.
- `ValidateStandardVTXOPolicy(nodes, operatorKey, minExitDelay)` — Admission
  check for standard Ark VTXO recipient policies. Wraps `ValidatePolicy` but
  requires a non-zero `minExitDelay`, failing closed so callers must specify
  an explicit floor rather than silently accepting any exit delay (including
  dangerously small values that would break forfeit incentives).
- `ValidatePolicy(nodes, opts)` — Structural admission check for any Ark policy
  shape (custom vHTLC, etc.). Enforces: collab leaf with operator key, exit
  leaf without operator key, no operator-unilateral leaf, CSV gating on exit
  paths. `opts.MinExitDelay = 0` skips the CSV minimum check.
- Decode budget constants: `MaxPolicyTemplateBytes` (64 KiB),
  `MaxLeafTemplateBytes` (16 KiB), `MaxPolicyLeaves` (32),
  `MaxPolicyDepth` (16), `MaxPolicyNodes` (256), `MaxMultisigKeys` (64) —
  cap decode work to prevent amplification from untrusted blobs.

## Relationships

- **Depends on**: (no internal repo imports; pure cryptographic library).
- **Depended on by**: nearly every protocol-logic package — `darepod`, `db`,
  `lib/tree`, `lib/types`, `lib/recovery`, `lib/tx` (and `arktx`,
  `checkpoint`, `oor`, `psbtutil` subpackages), `oor`, `round`, `txconfirm`,
  `unroll`, `vhtlcrecovery/unrollpolicy`, `sdk/swaps`, `vtxo`, `wallet`. It is
  the base script/policy layer.

## Invariants

- Node interface is sealed: only types defined in this package can implement it.
- Every collab leaf must contain the operator key for safe cosigning.
- Every exit leaf must be CSV-gated for unilateral recovery.
- No leaf may permit the operator to spend unilaterally: every `Multisig` node
  containing the operator key must contain at least one non-operator key.
  `Multisig{operator, operator}` is rejected because both signers resolve to the
  same party.
- CSV encoding uses `blockchain.LockTimeToSequence(false, exitDelay)` to store
  the BIP-68 block-mode sequence value, not the raw block count. Decoders that
  compare raw block counts against CSV lock values must convert accordingly.
- Canonical leaf ordering: sorted by version then lexicographic script bytes.
- All taproot outputs use the unspendable ARK NUMS key for key path (no
  key-path spend possible).
- Policy validation enforces at least one operator-containing leaf
  (collaborative) and at least one non-operator leaf (exit/unilateral).
- Exit delay must be >= `MinExitDelay` when `ValidateStandardVTXOPolicy` is
  used; `ValidatePolicy` with `MinExitDelay = 0` skips this check.
- Decode budget: policy template decode caps at `MaxPolicyTemplateBytes` /
  `MaxPolicyLeaves` / `MaxPolicyDepth` / `MaxPolicyNodes`; a single budget is
  shared across all leaves of one policy so an adversary cannot multiply work by
  packing many max-node leaves into one blob.

## Deep Docs

- [docs/arkscript_spec.md](../../docs/arkscript_spec.md) — RFC-style specification: AST grammar, encoding format, invariants, and security considerations.
- [docs/policy_arkscript_review_guide.md](../../docs/policy_arkscript_review_guide.md) — Policy-first reviewer guide for inspecting custom policies.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
