package arkscript

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
)

// PolicyLeaf represents a compiled tapscript leaf in canonical ordering.
type PolicyLeaf struct {
	// Leaf is the compiled tapscript leaf (script bytes + leaf version).
	Leaf txscript.TapLeaf
}

// CompareTo returns -1 if this leaf sorts before other, 1 if after, and 0 if
// they are equal. Ordering is by leaf version first, then lexicographic by
// script bytes.
func (l *PolicyLeaf) CompareTo(other *PolicyLeaf) int {
	if l.Leaf.LeafVersion != other.Leaf.LeafVersion {
		if l.Leaf.LeafVersion < other.Leaf.LeafVersion {
			return -1
		}

		return 1
	}

	return bytes.Compare(l.Leaf.Script, other.Leaf.Script)
}

// CompiledPolicy represents a fully compiled Ark policy with canonical leaf
// ordering, merkle tree structure, and derived spend information.
type CompiledPolicy struct {
	// InternalKey is the (unspendable) internal key for this policy.
	InternalKey *btcec.PublicKey

	// Leaves are the policy leaves in canonical order.
	Leaves []PolicyLeaf

	// RootHash is the merkle root of the canonical tap tree.
	RootHash []byte

	// leafHashes contains the TapLeaf hashes in canonical order. This is
	// computed during tree construction and cached for control block
	// derivation.
	leafHashes []chainhash.Hash

	// merkleProofs contains the inclusion proofs for each leaf. Each proof
	// is a list of sibling hashes from leaf to root.
	merkleProofs [][]chainhash.Hash
}

// OutputKey computes the taproot output key for this policy.
func (p *CompiledPolicy) OutputKey() *btcec.PublicKey {
	return txscript.ComputeTaprootOutputKey(p.InternalKey, p.RootHash)
}

// SpendInfo returns the spend information for the leaf at the given index.
func (p *CompiledPolicy) SpendInfo(leafIndex int) (*SpendInfo, error) {
	if leafIndex < 0 || leafIndex >= len(p.Leaves) {
		return nil, fmt.Errorf("leaf index %d out of bounds [0, %d)",
			leafIndex, len(p.Leaves))
	}

	// Build the control block for this leaf.
	controlBlock, err := p.buildControlBlock(leafIndex)
	if err != nil {
		return nil, fmt.Errorf("failed to build control block: %w", err)
	}

	leaf := &p.Leaves[leafIndex]

	return &SpendInfo{
		WitnessScript: bytes.Clone(leaf.Leaf.Script),
		ControlBlock:  controlBlock,
	}, nil
}

// buildControlBlock constructs the BIP-341 control block for the leaf at the
// given index. The merkle proof is built by our own tree walker (see
// BuildTree) because Ark's split-at-n/2 layout differs from txscript's
// pair-then-merge layout for non-power-of-two leaf counts; only the final
// serialization is delegated to txscript.ControlBlock.ToBytes so the exact
// byte layout stays anchored to the upstream taproot implementation.
func (p *CompiledPolicy) buildControlBlock(leafIndex int) ([]byte, error) {
	leaf := &p.Leaves[leafIndex]
	proof := p.merkleProofs[leafIndex]

	// Flatten the sibling hashes into the concatenated inclusion proof
	// form that ControlBlock expects.
	inclusionProof := make([]byte, 0, chainhash.HashSize*len(proof))
	for _, siblingHash := range proof {
		inclusionProof = append(inclusionProof, siblingHash[:]...)
	}

	// The output key parity bit is derived from the compressed SEC1
	// encoding: 0x02 is even, 0x03 is odd.
	outputKey := p.OutputKey()
	outputKeyYIsOdd := outputKey.SerializeCompressed()[0] == 0x03

	ctrlBlock := txscript.ControlBlock{
		InternalKey:     p.InternalKey,
		OutputKeyYIsOdd: outputKeyYIsOdd,
		LeafVersion:     leaf.Leaf.LeafVersion,
		InclusionProof:  inclusionProof,
	}

	return ctrlBlock.ToBytes()
}

// SpendInfo contains the witness-level data needed to spend a specific
// leaf in an Ark policy: the tapscript and its BIP-341 control block.
// Transaction-level context (nSequence, nLockTime) is NOT included here
// — that is derived from the AST node by the spending tx builder or
// carried on SpendPath for durable artifacts.
type SpendInfo struct {
	// WitnessScript is the tapscript leaf script bytes.
	WitnessScript []byte

	// ControlBlock is the BIP-341 control block for script-path spending.
	ControlBlock []byte
}

// SpendInfoForNode returns the witness-level spend info (script +
// control block) for the leaf corresponding to the given AST node.
// The canonical leaf index is resolved via ScriptIndex.
func (p *CompiledPolicy) SpendInfoForNode(node Node) (*SpendInfo, error) {
	script, err := node.Script()
	if err != nil {
		return nil, err
	}

	idx := p.ScriptIndex(script)
	if idx < 0 {
		return nil, fmt.Errorf("node script not found in tree")
	}

	return p.SpendInfo(idx)
}

// SpendPathForNode returns a full SpendPath for the leaf corresponding
// to the given AST node, including tx-context (sequence/locktime)
// derived from the AST and any provided condition witnesses.
func (p *CompiledPolicy) SpendPathForNode(node Node, conditions [][]byte) (
	*SpendPath, error) {

	info, err := p.SpendInfoForNode(node)
	if err != nil {
		return nil, err
	}

	return &SpendPath{
		SpendInfo:        info,
		RequiredSequence: DeriveSequence(node),
		RequiredLockTime: ExtractAbsoluteLockTime(node),
		Conditions:       cloneWitnessItems(conditions),
	}, nil
}

// ScriptIndex returns the canonical leaf index for the given script,
// or -1 if not found.
func (p *CompiledPolicy) ScriptIndex(script []byte) int {
	for i, leaf := range p.Leaves {
		if bytes.Equal(leaf.Leaf.Script, script) {
			return i
		}
	}

	return -1
}

// sortLeaves sorts the policy leaves in-place according to canonical
// ordering: leaf version first, then lexicographic by script bytes.
func sortLeaves(leaves []PolicyLeaf) {
	// Simple insertion sort for small leaf counts (typically 2-4 leaves).
	for i := 1; i < len(leaves); i++ {
		for j := i; j > 0 && leaves[j].CompareTo(
			&leaves[j-1],
		) < 0; j-- {

			leaves[j], leaves[j-1] = leaves[j-1], leaves[j]
		}
	}
}

// BuildTree constructs a canonical balanced binary taproot tree from the
// ordered leaf list using the Ark NUMS internal key. The NUMS key ensures
// key-path spends are impossible — all spending goes through script leaves.
//
// This implements the algorithm from the RFC:
//   - If n == 1: the root is the single leaf hash.
//   - If n > 1: split left = leaves[0:n/2], right = leaves[n/2:n],
//     compute L = BuildTree(left), R = BuildTree(right),
//     root = TapBranchHash(min(L,R), max(L,R)).
//
// The function also computes merkle proofs for each leaf.
func BuildTree(leaves []PolicyLeaf,
	internalKey *btcec.PublicKey) (*CompiledPolicy, error) {

	if len(leaves) == 0 {
		return nil, fmt.Errorf("cannot build tree with no leaves")
	}

	if internalKey == nil {
		return nil, fmt.Errorf("internal key must be provided")
	}

	// Enforce the Ark invariant: the internal key must be the
	// unspendable NUMS key. This prevents key-path spending and
	// guarantees all exits go through CSV-gated script leaves.
	if !internalKey.IsEqual(&ARKNUMSKey) {
		return nil, fmt.Errorf("internal key must be the Ark NUMS key")
	}

	// Defensively copy the input leaves so the compiled policy is
	// not aliased to the caller's slice, then sort canonically.
	// This is the single canonical ordering point for all Ark
	// policies — callers do not need to pre-sort.
	copied := make([]PolicyLeaf, len(leaves))
	for i, leaf := range leaves {
		copied[i] = PolicyLeaf{
			Leaf: txscript.TapLeaf{
				LeafVersion: leaf.Leaf.LeafVersion,
				Script:      bytes.Clone(leaf.Leaf.Script),
			},
		}
	}
	leaves = copied

	sortLeaves(leaves)

	// Compute leaf hashes.
	leafHashes := make([]chainhash.Hash, len(leaves))
	for i, leaf := range leaves {
		leafHashes[i] = leaf.Leaf.TapHash()
	}

	// Initialize merkle proofs (one per leaf, initially empty).
	merkleProofs := make([][]chainhash.Hash, len(leaves))
	for i := range merkleProofs {
		merkleProofs[i] = make([]chainhash.Hash, 0)
	}

	// Build the tree recursively, collecting proofs along the way.
	rootHash := buildTreeRecursive(leafHashes, 0, merkleProofs)

	return &CompiledPolicy{
		InternalKey:  internalKey,
		Leaves:       leaves,
		RootHash:     rootHash[:],
		leafHashes:   leafHashes,
		merkleProofs: merkleProofs,
	}, nil
}

// buildTreeRecursive builds the merkle tree and collects proofs.
// - hashes: the node hashes at the current level.
// - startIndex: the starting leaf index for this subtree.
// - proofs: accumulator for merkle proofs (modified in place).
//
// Returns the root hash of this subtree.
func buildTreeRecursive(hashes []chainhash.Hash, startIndex int,
	proofs [][]chainhash.Hash) chainhash.Hash {

	n := len(hashes)
	if n == 1 {
		return hashes[0]
	}

	// Split into left and right halves.
	mid := n / 2
	leftHashes := hashes[:mid]
	rightHashes := hashes[mid:]

	// Recursively build left and right subtrees.
	leftRoot := buildTreeRecursive(leftHashes, startIndex, proofs)
	rightRoot := buildTreeRecursive(rightHashes, startIndex+mid, proofs)

	// Add sibling hashes to proofs for leaves in each subtree.
	// Left subtree leaves get the right root as sibling.
	for i := 0; i < mid; i++ {
		proofs[startIndex+i] = append(proofs[startIndex+i], rightRoot)
	}

	// Right subtree leaves get the left root as sibling.
	for i := mid; i < n; i++ {
		proofs[startIndex+i] = append(proofs[startIndex+i], leftRoot)
	}

	// Compute the branch hash. BIP-341 specifies sorting: hash(min || max).
	return tapBranchHash(leftRoot, rightRoot)
}

// tapBranchHash computes the BIP-341 TapBranch hash for two child hashes.
// Per BIP-341: hash(min(a,b) || max(a,b)).
func tapBranchHash(a, b chainhash.Hash) chainhash.Hash {
	// Sort lexicographically.
	if bytes.Compare(a[:], b[:]) > 0 {
		a, b = b, a
	}

	// The tag for TapBranch is "TapBranch".
	return *chainhash.TaggedHash(chainhash.TagTapBranch, a[:], b[:])
}
