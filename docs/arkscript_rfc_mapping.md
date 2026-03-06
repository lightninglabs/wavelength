# RFC-to-Implementation Mapping: Ark Closure AST (PR #58)

This document provides a comprehensive mapping between the RFC requirements in `client/docs/vtxo_ast_plan.md` and the implementation in `client/lib/arkscript/`.

---

## Executive Summary

The implementation in `lib/arkscript/` fulfills all RFC requirements. Key deliverables:

| Component | Status | Files |
|-----------|--------|-------|
| AST Node Types | ✅ Complete | `node.go` |
| Canonical Encoding | ✅ Complete | `node.go` |
| Leaf Ordering | ✅ Complete | `tree.go` |
| Tree Construction | ✅ Complete | `tree.go` |
| SpendInfo API | ✅ Complete | `tree.go`, `vtxo.go` |
| Tx-Context Derivation | ✅ Complete | `vtxo.go` |
| VTXO Validation | ✅ Complete | `vtxo.go` |
| PSBT Extensions | ✅ Complete | `psbt.go` |
| Assets Composition | ✅ Complete | `compose.go` |
| Golden Tests | ✅ Complete | `golden_test.go` |
| Backward Compatibility | ✅ Verified | All `*_test.go` |
| vHTLC Example | ✅ Complete | `vhtlc_test.go` |

---

## 1. Package Structure

### RFC Requirement
> **PR #58 MUST:**
> - Add the new implementation under `client/lib/arkscript/`.
> - Keep `client/lib/scripts/` as the stable public surface for other repositories.
> - Reuse the existing NUMS/unspendable key machinery in `client/lib/scripts/nums.go` as the default internal key provider.

### Implementation
```
client/lib/arkscript/
├── node.go         # AST node definitions
├── node_test.go    # Node encoding tests
├── tree.go         # Tree builder and control blocks
├── tree_test.go    # Tree construction tests
├── vtxo.go         # VTXO policy builder
├── vtxo_test.go    # VTXO policy tests
├── golden_test.go  # Backward compatibility vectors
├── psbt.go         # PSBT encoding helpers
├── psbt_test.go    # PSBT encoding tests
├── compose.go      # Assets composition API
├── compose_test.go # Composition tests
└── vhtlc_test.go   # vHTLC example (complex composition)
```

The NUMS key is imported from `lib/scripts/nums.go`:
```go
// vtxo.go:122
policy, err := BuildTree(leaves, &scripts.ARKNUMSKey)
```

---

## 2. AST Node Types

### RFC Requirement
> **The node set below MUST be sufficient to express required Ark policies:**
> - `Checksig(key)`: one schnorr signature required.
> - `Multisig(keys, type)`: N-of-N schnorr signatures required.
> - `CSV(lock, inner)`: relative timelock gate (BIP-68 + OP_CSV).
> - `CLTV(lock, inner)`: absolute timelock gate (BIP-65 + OP_CLTV).
> - `HashLock(algo, hash, inner)`: preimage gate (HASH160/SHA256).

### Implementation (`node.go`)

All five node types are implemented with a sealed `Node` interface:

```go
// Node is the interface that all AST nodes must implement.
type Node interface {
    Script() ([]byte, error)
    nodeSealed()  // Marker to prevent external implementations
}
```

| Node | Lines | Implementation |
|------|-------|----------------|
| `Checksig` | 47-68 | Single x-only pubkey + CHECKSIG |
| `Multisig` | 70-145 | CHECKSIGVERIFY chain or CHECKSIGADD |
| `CSV` | 147-181 | Inner expression + lock + CSV + DROP |
| `CLTV` | 183-216 | Lock + CLTV + DROP + inner expression |
| `HashLock` | 218-286 | Hash op + hash + EQUALVERIFY + inner |

---

## 3. Canonical Script Encoding

### 3.1 Checksig

**RFC:**
> `<xonly_pubkey> OP_CHECKSIG`

**Implementation (`node.go:55-64`):**
```go
func (c *Checksig) Script() ([]byte, error) {
    builder := txscript.NewScriptBuilder()
    builder.AddData(schnorr.SerializePubKey(c.Key))  // x-only (32 bytes)
    builder.AddOp(txscript.OP_CHECKSIG)
    return builder.Script()
}
```

### 3.2 Multisig (CHECKSIGVERIFY chain)

**RFC:**
> ```
> <k0> OP_CHECKSIGVERIFY
> <k1> OP_CHECKSIGVERIFY
> ...
> <k[n-1]> OP_CHECKSIG
> ```

**Implementation (`node.go:104-121`):**
```go
func (m *Multisig) scriptChecksig() ([]byte, error) {
    builder := txscript.NewScriptBuilder()
    for i, key := range m.Keys {
        builder.AddData(schnorr.SerializePubKey(key))
        if i < len(m.Keys)-1 {
            builder.AddOp(txscript.OP_CHECKSIGVERIFY)
        } else {
            builder.AddOp(txscript.OP_CHECKSIG)
        }
    }
    return builder.Script()
}
```

### 3.3 CSV

**RFC:**
> ```
> <inner>
> <lock> OP_CHECKSEQUENCEVERIFY OP_DROP
> ```

**Implementation (`node.go:158-178`):**
```go
func (c *CSV) Script() ([]byte, error) {
    innerScript, err := c.Inner.Script()
    builder := txscript.NewScriptBuilder()
    builder.AddOps(innerScript)                        // <inner>
    builder.AddInt64(int64(c.Lock))                   // <lock>
    builder.AddOp(txscript.OP_CHECKSEQUENCEVERIFY)   // OP_CSV
    builder.AddOp(txscript.OP_DROP)                   // OP_DROP
    return builder.Script()
}
```

### 3.4 CLTV

**RFC:**
> ```
> <lock> OP_CHECKLOCKTIMEVERIFY OP_DROP
> <inner>
> ```

**Implementation (`node.go:194-212`):**
```go
func (c *CLTV) Script() ([]byte, error) {
    innerScript, err := c.Inner.Script()
    builder := txscript.NewScriptBuilder()
    builder.AddInt64(int64(c.Lock))                  // <lock>
    builder.AddOp(txscript.OP_CHECKLOCKTIMEVERIFY)  // OP_CLTV
    builder.AddOp(txscript.OP_DROP)                  // OP_DROP
    builder.AddOps(innerScript)                       // <inner>
    return builder.Script()
}
```

### 3.5 HashLock

**RFC:**
> **SHA256:** `OP_SHA256 <32-byte-hash> OP_EQUALVERIFY <inner>`
> **HASH160:** `OP_HASH160 <20-byte-hash> OP_EQUALVERIFY <inner>`

**Implementation (`node.go:234-283`):**
```go
func (h *HashLock) Script() ([]byte, error) {
    // Validate hash length based on algorithm
    innerScript, err := h.Inner.Script()

    builder := txscript.NewScriptBuilder()
    switch h.Algorithm {
    case HashAlgoHash160:
        builder.AddOp(txscript.OP_HASH160)
    case HashAlgoSHA256:
        builder.AddOp(txscript.OP_SHA256)
    }
    builder.AddData(h.Hash)
    builder.AddOp(txscript.OP_EQUALVERIFY)
    builder.AddOps(innerScript)
    return builder.Script()
}
```

---

## 4. Leaf Ordering

### RFC Requirement
> **Leaf ordering MUST be:**
> - primary: `family_role_rank`,
> - secondary: lexicographic ordering of `leaf_script_bytes`.
>
> **Role ranks:** `collab`=0, `exit`=1, `custom`=2.

### Implementation (`tree.go:14-79`)

```go
type LeafRole int

const (
    LeafRoleCollab LeafRole = iota  // Rank 0
    LeafRoleExit                     // Rank 1
    LeafRoleCustom                   // Rank 2
)

func (l *PolicyLeaf) CompareTo(other *PolicyLeaf) int {
    // Primary: role rank
    if l.Role.Rank() < other.Role.Rank() {
        return -1
    }
    if l.Role.Rank() > other.Role.Rank() {
        return 1
    }
    // Secondary: lexicographic script bytes
    return bytes.Compare(l.Leaf.Script, other.Leaf.Script)
}
```

---

## 5. Tree Construction

### RFC Requirement
> **Define `BuildTree(leaves)` recursively:**
> - If `n == 1`: the root is the single leaf hash.
> - If `n > 1`: split, compute subtrees, `root = TapBranchHash(min(L,R), max(L,R))`.

### Implementation (`tree.go:198-285`)

```go
func BuildTree(leaves []PolicyLeaf, internalKey *btcec.PublicKey) (
    *CompiledPolicy, error) {

    leafHashes := make([]chainhash.Hash, len(leaves))
    for i, leaf := range leaves {
        leafHashes[i] = leaf.Leaf.TapHash()  // BIP-341 tapleaf hash
    }

    rootHash := buildTreeRecursive(leafHashes, 0, merkleProofs)
    return &CompiledPolicy{...}, nil
}

func buildTreeRecursive(hashes []chainhash.Hash, startIndex int,
    proofs [][]chainhash.Hash) chainhash.Hash {

    n := len(hashes)
    if n == 1 {
        return hashes[0]
    }

    mid := n / 2
    leftRoot := buildTreeRecursive(hashes[:mid], startIndex, proofs)
    rightRoot := buildTreeRecursive(hashes[mid:], startIndex+mid, proofs)

    // Collect sibling proofs for control blocks
    for i := 0; i < mid; i++ {
        proofs[startIndex+i] = append(proofs[startIndex+i], rightRoot)
    }
    for i := mid; i < n; i++ {
        proofs[startIndex+i] = append(proofs[startIndex+i], leftRoot)
    }

    return tapBranchHash(leftRoot, rightRoot)
}

// BIP-341: hash(min(a,b) || max(a,b))
func tapBranchHash(a, b chainhash.Hash) chainhash.Hash {
    if bytes.Compare(a[:], b[:]) > 0 {
        a, b = b, a
    }
    return *chainhash.TaggedHash(chainhash.TagTapBranch, a[:], b[:])
}
```

---

## 6. SpendInfo API

### RFC Requirement
> **The `SpendInfo` type MUST include:**
> - `WitnessScript []byte`
> - `ControlBlock []byte`
> - `RequiredSequence uint32`
> - `RequiredLockTime uint32`

### Implementation (`tree.go:167-184`)

```go
type SpendInfo struct {
    WitnessScript    []byte   // Tapscript leaf script bytes
    ControlBlock     []byte   // BIP-341 control block
    RequiredSequence uint32   // BIP-68 sequence value
    RequiredLockTime uint32   // nLockTime value
}
```

**Control block construction (`tree.go:131-165`):**
- 1 byte: control byte (leaf version + parity)
- 32 bytes: internal key (x-only)
- 32 * n bytes: merkle proof siblings

---

## 7. Transaction-Context Derivation

### RFC Requirement
> **`RequiredSequence`:**
> - CSV: exact BIP-68 lock value
> - CLTV only: `0xfffffffe`
> - Neither: `0xffffffff`
>
> **`RequiredLockTime`:**
> - CLTV: lock value
> - Otherwise: `0`

### Implementation (`vtxo.go:135-214`)

```go
func DeriveSequence(node Node) uint32 {
    csvLock, hasCLTV := extractTimelocks(node)
    if csvLock > 0 {
        return csvLock
    }
    if hasCLTV {
        return 0xfffffffe
    }
    return 0xffffffff
}

func DeriveLockTime(node Node) uint32 {
    _, cltvLock := extractCLTV(node)
    return cltvLock
}
```

**Tested in `vtxo_test.go:198-293`** with cases for simple checksig, CSV, CLTV, nested combinations, and hashlock preservation.

---

## 8. VTXO Policy Validation

### RFC Requirement
> **A VTXO policy MUST:**
> - contain at least one `collab` leaf (requires operator key)
> - contain at least one `exit` leaf (CSV-gated, no operator key)

### Implementation (`vtxo.go:248-275`)

```go
func ValidateVTXOLeaves(leaves []PolicyLeaf) error {
    hasCollab, hasExit := false, false
    for _, leaf := range leaves {
        switch leaf.Role {
        case LeafRoleCollab:
            hasCollab = true
        case LeafRoleExit:
            hasExit = true
        }
    }
    if !hasCollab {
        return ErrMissingCollab
    }
    if !hasExit {
        return ErrMissingExit
    }
    return nil
}
```

### RFC Requirement
> **Exported, comparable error classes** with stable codes.

### Implementation (`vtxo.go:216-246`)

```go
type VTXOValidationError struct {
    Code    string
    Message string
}

var (
    ErrMissingCollab = &VTXOValidationError{
        Code:    "MISSING_COLLAB",
        Message: "VTXO policy must contain a collab leaf",
    }
    ErrMissingExit = &VTXOValidationError{
        Code:    "MISSING_EXIT",
        Message: "VTXO policy must contain an exit leaf",
    }
)
```

---

## 9. PSBT Extensions

### RFC Requirement
> **Encoding:**
> - leaf count (compact size uint)
> - per leaf: depth (1 byte), version (1 byte), script length (compact size), script bytes
>
> **Key namespace:** `ark/taptree`, `ark/condition`

### Implementation (`psbt.go`)

```go
const PSBTKeyPrefix = "ark/"
const PSBTKeyTapTree = PSBTKeyPrefix + "taptree"
const PSBTKeyConditionWitness = PSBTKeyPrefix + "condition"

func EncodeTapTree(policy *CompiledPolicy) ([]byte, error) {
    var buf bytes.Buffer
    writeCompactSize(&buf, uint64(len(policy.Leaves)))
    for i, leaf := range policy.Leaves {
        buf.WriteByte(depths[i])
        buf.WriteByte(byte(leaf.Leaf.LeafVersion))
        writeCompactSize(&buf, uint64(len(leaf.Leaf.Script)))
        buf.Write(leaf.Leaf.Script)
    }
    return buf.Bytes(), nil
}

func DecodeTapTree(data []byte) ([]EncodedLeaf, error) {
    // Inverse of EncodeTapTree
}

func EncodeConditionWitness(preimage []byte) []byte {
    // Standard witness serialization
}

func DecodeConditionWitness(data []byte) ([]byte, error) {
    // Inverse
}
```

**Tested in `psbt_test.go`** with round-trip, format validation, empty/truncated data, and large scripts.

---

## 10. Assets Composition

### RFC Requirement
> **Composition API:**
> - Compile Ark policy into policy root
> - Combine with external root: `TapBranchHash(min(policyRoot, extRoot), max(policyRoot, extRoot))`
> - Output key tweak uses combined root

### Implementation (`compose.go`)

```go
type ComposedPolicy struct {
    InternalKey  *btcec.PublicKey
    PolicyRoot   chainhash.Hash
    ExternalRoot chainhash.Hash
    CombinedRoot chainhash.Hash
    ArkPolicy    *CompiledPolicy
}

func ComposeWithSiblingRoot(
    policy *CompiledPolicy,
    externalRoot chainhash.Hash,
) (*ComposedPolicy, error) {

    var policyRoot chainhash.Hash
    copy(policyRoot[:], policy.RootHash)

    combinedRoot := tapBranchHashCompose(policyRoot, externalRoot)

    return &ComposedPolicy{
        InternalKey:  policy.InternalKey,
        PolicyRoot:   policyRoot,
        ExternalRoot: externalRoot,
        CombinedRoot: combinedRoot,
        ArkPolicy:    policy,
    }, nil
}

func (c *ComposedPolicy) SpendInfo(leafIndex int) (*SpendInfo, error) {
    // Original spend info with external root appended to control block
    // Control block grows by 32 bytes (external root as extra sibling)
}
```

**Tested in `compose_test.go`** with output key changes, control block length, determinism, and BIP-341 ordering.

---

## 11. Golden Tests (Backward Compatibility)

### RFC Requirement
> **Golden test vectors MUST capture:**
> - Default VTXO output key for known inputs
> - Exact script bytes for exit and collab leaves
> - Control block bytes for each leaf

### Implementation (`golden_test.go:41-81`)

```go
var goldenVTXOVectors = []GoldenVTXOVector{
    {
        Name:              "standard_vtxo_delay_100",
        OwnerKeyIndex:     1,
        OperatorKeyIndex:  2,
        ExitDelay:         100,
        OutputKeyHex:      "034b1da83aada85e6879ef9b6b2d6cb0a5ae6d6e...",
        CollabScriptHex:   "20c6047f9441ed7d6d3045406e95c07cd85c778e...",
        TimeoutScriptHex:  "20c6047f9441ed7d6d3045406e95c07cd85c778e...",
        CollabControlHex:  "c1372f225b3caee8213096de3229ee4335306b07...",
        TimeoutControlHex: "c1372f225b3caee8213096de3229ee4335306b07...",
        RootHashHex:       "9556f4e03cfb047183ca430d888510c81f85d290...",
        InternalKeyHex:    "02372f225b3caee8213096de3229ee4335306b07...",
    },
    // Additional vectors for delay_144, different_keys_delay_1000
}
```

**Cross-validation (`vtxo_test.go:79-134`):**
```go
func TestNewVTXOPolicyMatchesScriptsPackage(t *testing.T) {
    // Build using arkscript
    policy, err := NewVTXOPolicy(ownerKey, operatorKey, exitDelay)

    // Build using scripts package (current implementation)
    tapscript, err := scripts.VTXOTapScript(ownerKey, operatorKey, exitDelay)

    // Verify byte-identical outputs
    require.Equal(t, taprootKey.SerializeCompressed(),
        policy.OutputKey().SerializeCompressed())
    require.Equal(t, tapscript.RootHash, policy.RootHash)
    require.Equal(t, tapscript.Leaves[0].Script, policy.Leaves[0].Leaf.Script)
    require.Equal(t, tapscript.Leaves[1].Script, policy.Leaves[1].Leaf.Script)
    require.Equal(t, scriptsCollabInfo.ControlBlock, arkscriptCollabInfo.ControlBlock)
    require.Equal(t, scriptsExitInfo.ControlBlock, arkscriptExitInfo.ControlBlock)
}
```

---

## 12. Default VTXO Template

### RFC Requirement
> **`CSV(lock, Checksig(key))`** MUST produce:
> ```
> <xonly_key> OP_CHECKSIG <lock> OP_CHECKSEQUENCEVERIFY OP_DROP
> ```
>
> **Collab leaf key order:** `[owner, operator]`

### Implementation (`vtxo.go:66-133`)

```go
func NewVTXOPolicy(ownerKey, operatorKey *btcec.PublicKey,
    exitDelay uint32) (*VTXOPolicy, error) {

    // Collab leaf: Multisig([owner, operator])
    collabNode := &Multisig{
        Keys: []*btcec.PublicKey{ownerKey, operatorKey},
        Type: MultisigTypeChecksig,
    }
    collabScript, _ := collabNode.Script()

    // Exit leaf: CSV(delay, Checksig(owner))
    exitNode := &CSV{
        Lock:  exitDelay,
        Inner: &Checksig{Key: ownerKey},
    }
    exitScript, _ := exitNode.Script()

    // Canonical order: collab=0, exit=1
    leaves := []PolicyLeaf{
        {Role: LeafRoleCollab, Leaf: txscript.NewBaseTapLeaf(collabScript)},
        {Role: LeafRoleExit, Leaf: txscript.NewBaseTapLeaf(exitScript)},
    }

    policy, _ := BuildTree(leaves, &scripts.ARKNUMSKey)
    return &VTXOPolicy{CompiledPolicy: policy, ...}, nil
}
```

---

## 13. Leaf Version

### RFC Requirement
> **Base tapleaf version (BIP-341)** for all compiled leaves.

### Implementation

All leaves use `txscript.NewBaseTapLeaf()`:
```go
leaves := []PolicyLeaf{
    {Role: LeafRoleCollab, Leaf: txscript.NewBaseTapLeaf(collabScript)},
    {Role: LeafRoleExit, Leaf: txscript.NewBaseTapLeaf(exitScript)},
}
```

**Verified in `golden_test.go:301-306`:**
```go
require.Equal(t, txscript.BaseLeafVersion, tapscript.Leaves[0].LeafVersion)
require.Equal(t, txscript.BaseLeafVersion, tapscript.Leaves[1].LeafVersion)
```

---

## 14. Unspendable Internal Key

### RFC Requirement
> **Key-path spends SHOULD be disabled** via unspendable internal key (NUMS).

### Implementation

The NUMS key from `lib/scripts/nums.go` is used:
```go
// vtxo.go:122
policy, err := BuildTree(leaves, &scripts.ARKNUMSKey)
```

**Verified in `golden_test.go:317-336`:**
```go
func TestNUMSKeyConsistency(t *testing.T) {
    expectedHex := "02372f225b3caee8213096de3229ee4335306b07c3c169438461b5d4749884ec65"
    actualHex := hex.EncodeToString(scripts.ARKNUMSKey.SerializeCompressed())
    require.Equal(t, expectedHex, actualHex)
}
```

---

## 15. vHTLC Example: Complex AST Composition

The vHTLC (virtual HTLC) demonstrates the full composability of the AST system. It uses all five node types across 6 leaves, showcasing how complex policies can be built from simple primitives.

### vHTLC Leaf Structure

| # | Name | AST Composition | Purpose |
|---|------|--------------------|---------|
| 1 | Claim | `HashLock(hash, Multisig([receiver, server]))` | Receiver claims with preimage + server cosign |
| 2 | Refund | `Multisig([sender, receiver, server])` | All parties agree to refund |
| 3 | RefundWithoutReceiver | `CLTV(locktime, Multisig([sender, server]))` | Refund after timeout without receiver |
| 4 | UnilateralClaim | `CSV(delay, HashLock(hash, Checksig(receiver)))` | Receiver exits with preimage |
| 5 | UnilateralRefund | `CSV(delay, Multisig([sender, receiver]))` | Sender+receiver exit together |
| 6 | UnilateralRefundWithoutReceiver | `CSV(delay, Checksig(sender))` | Sender exits alone (longest delay) |

### Implementation (`vhtlc_test.go`)

```go
type VHTLCOpts struct {
    Sender                               *btcec.PublicKey
    Receiver                             *btcec.PublicKey
    Server                               *btcec.PublicKey
    PreimageHash                         []byte  // HASH160, 20 bytes
    RefundLocktime                       uint32  // Absolute (CLTV)
    UnilateralClaimDelay                 uint32  // Relative (CSV)
    UnilateralRefundDelay                uint32  // Relative (CSV)
    UnilateralRefundWithoutReceiverDelay uint32  // Relative (CSV)
}

func NewVHTLCPolicy(opts VHTLCOpts) (*VHTLCPolicy, error) {
    // 1. Claim: HashLock + Multisig([receiver, server])
    claimClosure := &HashLock{
        Algorithm: HashAlgoHash160,
        Hash:      opts.PreimageHash,
        Inner: &Multisig{
            Keys: []*btcec.PublicKey{opts.Receiver, opts.Server},
            Type: MultisigTypeChecksig,
        },
    }

    // 2. Refund: Multisig([sender, receiver, server])
    refundClosure := &Multisig{
        Keys: []*btcec.PublicKey{opts.Sender, opts.Receiver, opts.Server},
        Type: MultisigTypeChecksig,
    }

    // 3. RefundWithoutReceiver: CLTV + Multisig([sender, server])
    refundWithoutReceiverClosure := &CLTV{
        Lock: opts.RefundLocktime,
        Inner: &Multisig{
            Keys: []*btcec.PublicKey{opts.Sender, opts.Server},
            Type: MultisigTypeChecksig,
        },
    }

    // 4. UnilateralClaim: CSV + HashLock + Checksig(receiver)
    unilateralClaimClosure := &CSV{
        Lock: opts.UnilateralClaimDelay,
        Inner: &HashLock{
            Algorithm: HashAlgoHash160,
            Hash:      opts.PreimageHash,
            Inner:     &Checksig{Key: opts.Receiver},
        },
    }

    // 5. UnilateralRefund: CSV + Multisig([sender, receiver])
    unilateralRefundClosure := &CSV{
        Lock: opts.UnilateralRefundDelay,
        Inner: &Multisig{
            Keys: []*btcec.PublicKey{opts.Sender, opts.Receiver},
            Type: MultisigTypeChecksig,
        },
    }

    // 6. UnilateralRefundWithoutReceiver: CSV + Checksig(sender)
    unilateralRefundWithoutReceiverClosure := &CSV{
        Lock: opts.UnilateralRefundWithoutReceiverDelay,
        Inner: &Checksig{Key: opts.Sender},
    }

    // Build leaves with appropriate roles
    leaves := []PolicyLeaf{
        {Role: LeafRoleCollab, Leaf: txscript.NewBaseTapLeaf(claimScript)},
        {Role: LeafRoleCollab, Leaf: txscript.NewBaseTapLeaf(refundScript)},
        {Role: LeafRoleCollab, Leaf: txscript.NewBaseTapLeaf(refundWithoutReceiverScript)},
        {Role: LeafRoleExit, Leaf: txscript.NewBaseTapLeaf(unilateralClaimScript)},
        {Role: LeafRoleExit, Leaf: txscript.NewBaseTapLeaf(unilateralRefundScript)},
        {Role: LeafRoleExit, Leaf: txscript.NewBaseTapLeaf(unilateralRefundWithoutReceiverScript)},
    }

    SortLeaves(leaves)
    return BuildTree(leaves, &scripts.ARKNUMSKey)
}
```

### Compiled Scripts

The AST nodes compile to the following tapscripts:

```
Claim (HashLock+Multisig):
  OP_HASH160 <hash> OP_EQUALVERIFY <receiver> OP_CHECKSIGVERIFY <server> OP_CHECKSIG

Refund (Multisig):
  <sender> OP_CHECKSIGVERIFY <receiver> OP_CHECKSIGVERIFY <server> OP_CHECKSIG

RefundWithoutReceiver (CLTV+Multisig):
  <locktime> OP_CLTV OP_DROP <sender> OP_CHECKSIGVERIFY <server> OP_CHECKSIG

UnilateralClaim (CSV+HashLock+Checksig):
  OP_HASH160 <hash> OP_EQUALVERIFY <receiver> OP_CHECKSIG <delay> OP_CSV OP_DROP

UnilateralRefund (CSV+Multisig):
  <sender> OP_CHECKSIGVERIFY <receiver> OP_CHECKSIG <delay> OP_CSV OP_DROP

UnilateralRefundWithoutReceiver (CSV+Checksig):
  <sender> OP_CHECKSIG <delay> OP_CSV OP_DROP
```

### Tx-Context Derivation

Each leaf automatically derives its transaction context requirements:

| Leaf | RequiredSequence | RequiredLockTime |
|------|------------------|------------------|
| Claim | `0xffffffff` | `0` |
| Refund | `0xffffffff` | `0` |
| RefundWithoutReceiver | `0xfffffffe` | `<locktime>` |
| UnilateralClaim | `<delay>` | `0` |
| UnilateralRefund | `<delay>` | `0` |
| UnilateralRefundWithoutReceiver | `<delay>` | `0` |

### Test Coverage

The vHTLC test suite (`vhtlc_test.go`) verifies:
- Policy construction with all 6 leaves
- Canonical leaf ordering (collab leaves first, then exit leaves)
- SpendInfo for each leaf (script, control block)
- Tx-context derivation for each leaf
- Deterministic output keys
- Composition with external roots (Taproot Assets)

```bash
# Run vHTLC tests
make unit pkg=./lib/arkscript case=TestVHTLC timeout=5m
```

---

## Summary: Complete RFC Coverage

| RFC Section | Status | Implementation |
|-------------|--------|----------------|
| Package structure | ✅ | `lib/arkscript/` |
| AST node types | ✅ | `node.go` (5 types) |
| Canonical script encoding | ✅ | `node.go` (all encodings) |
| Leaf ordering | ✅ | `tree.go` (role rank + lex) |
| Tree construction | ✅ | `tree.go` (BuildTree) |
| SpendInfo API | ✅ | `tree.go`, `vtxo.go` |
| Tx-context derivation | ✅ | `vtxo.go` (DeriveSequence/LockTime) |
| VTXO validation | ✅ | `vtxo.go` (ValidateVTXOLeaves) |
| Error taxonomy | ✅ | `vtxo.go` (VTXOValidationError) |
| PSBT extensions | ✅ | `psbt.go` (Encode/Decode) |
| Assets composition | ✅ | `compose.go` (ComposeWithSiblingRoot) |
| Golden tests | ✅ | `golden_test.go` |
| Backward compatibility | ✅ | `vtxo_test.go` (cross-validation) |
| vHTLC example | ✅ | `vhtlc_test.go` (complex composition) |

---

## Test Commands

```bash
# Run all arkscript tests
make unit pkg=./lib/arkscript timeout=5m

# Run golden vector tests specifically
make unit pkg=./lib/arkscript case=TestGoldenVTXOVectors

# Run cross-validation test
make unit pkg=./lib/arkscript case=TestNewVTXOPolicyMatchesScriptsPackage

# Run with verbose output
make unit-debug log="stdlog trace" pkg=./lib/arkscript

# Lint
make lint
```

---

## Files Modified/Created

| File | Purpose |
|------|---------|
| `lib/arkscript/node.go` | AST node types and canonical encoding |
| `lib/arkscript/node_test.go` | Node encoding tests |
| `lib/arkscript/tree.go` | Tree builder, leaf ordering, SpendInfo |
| `lib/arkscript/tree_test.go` | Tree construction tests |
| `lib/arkscript/vtxo.go` | VTXO policy, validation, tx-context |
| `lib/arkscript/vtxo_test.go` | VTXO policy and cross-validation tests |
| `lib/arkscript/golden_test.go` | Golden vectors for backward compat |
| `lib/arkscript/psbt.go` | PSBT tap tree encoding |
| `lib/arkscript/psbt_test.go` | PSBT encoding tests |
| `lib/arkscript/compose.go` | Assets composition API |
| `lib/arkscript/compose_test.go` | Composition tests |
| `lib/arkscript/vhtlc_test.go` | vHTLC example demonstrating full AST usage |
