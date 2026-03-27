package arkscript

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
)

// LeafRole classifies a policy leaf as collaborative or unilateral exit.
type LeafRole uint8

const (
	// LeafRoleCollab indicates a collaborative spending path that
	// requires the operator as a cosigner.
	LeafRoleCollab LeafRole = iota

	// LeafRoleExit indicates a unilateral exit path that does not
	// require operator cooperation.
	LeafRoleExit
)

// String returns a human-readable label for the leaf role.
func (r LeafRole) String() string {
	switch r {
	case LeafRoleCollab:
		return "collab"
	case LeafRoleExit:
		return "exit"
	default:
		return "unknown"
	}
}

// PolicyLeaf represents a compiled tapscript leaf in canonical ordering.
type PolicyLeaf struct {
	// Leaf is the compiled tapscript leaf (script bytes + leaf version).
	Leaf txscript.TapLeaf

	// Role classifies this leaf as collaborative or unilateral exit.
	Role LeafRole
}

// CompareTo returns -1 if this leaf sorts before other, 1 if after, and 0 if
// they are equal. Ordering is lexicographic by script bytes.
func (l *PolicyLeaf) CompareTo(other *PolicyLeaf) int {
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
		WitnessScript:    leaf.Leaf.Script,
		ControlBlock:     controlBlock,
		RequiredSequence: 0xffffffff, // Default; caller may override.
		RequiredLockTime: 0,
	}, nil
}

// buildControlBlock constructs the BIP-341 control block for the leaf at the
// given index.
func (p *CompiledPolicy) buildControlBlock(leafIndex int) ([]byte, error) {
	leaf := &p.Leaves[leafIndex]
	proof := p.merkleProofs[leafIndex]

	// Determine output key parity for the control byte.
	outputKey := p.OutputKey()
	outputKeyParity := outputKey.SerializeCompressed()[0] == 0x03

	// Control block format:
	// - 1 byte: control byte (leaf version + output key parity)
	// - 32 bytes: internal key (x-only)
	// - 32 * n bytes: merkle proof (sibling hashes)
	controlBlockLen := 1 + 32 + 32*len(proof)
	controlBlock := make([]byte, 0, controlBlockLen)

	// Control byte: leaf version | (parity << 0).
	controlByte := byte(leaf.Leaf.LeafVersion)
	if outputKeyParity {
		controlByte |= 0x01
	}
	controlBlock = append(controlBlock, controlByte)

	// Append internal key (x-only, 32 bytes).
	internalKeyBytes := p.InternalKey.SerializeCompressed()[1:]
	controlBlock = append(controlBlock, internalKeyBytes...)

	// Append merkle proof (sibling hashes from leaf to root).
	for _, siblingHash := range proof {
		controlBlock = append(controlBlock, siblingHash[:]...)
	}

	return controlBlock, nil
}

// SpendInfo contains all the information needed to spend a specific leaf in
// an Ark policy.
type SpendInfo struct {
	// WitnessScript is the tapscript leaf script bytes.
	WitnessScript []byte

	// ControlBlock is the BIP-341 control block for script-path spending.
	ControlBlock []byte

	// RequiredSequence is the BIP-68 sequence value required for this leaf.
	// Policy-specific builders may override this from the default max
	// sequence when the selected leaf requires CSV or non-final sequence.
	RequiredSequence uint32

	// RequiredLockTime is the nLockTime value required for this leaf.
	// Policy-specific builders may override this when the selected leaf
	// requires an absolute locktime.
	RequiredLockTime uint32
}

// SortLeaves sorts the policy leaves in-place according to canonical
// lexicographic ordering of script bytes.
func SortLeaves(leaves []PolicyLeaf) {
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
// ordered leaf list. This implements the algorithm from the RFC:
//   - If n == 1: the root is the single leaf hash.
//   - If n > 1: split left = leaves[0:n/2], right = leaves[n/2:n],
//     compute L = BuildTree(left), R = BuildTree(right),
//     root = TapBranchHash(min(L,R), max(L,R)).
//
// The function also computes merkle proofs for each leaf.
func BuildTree(leaves []PolicyLeaf, internalKey *btcec.PublicKey) (
	*CompiledPolicy, error) {

	if len(leaves) == 0 {
		return nil, fmt.Errorf("cannot build tree with no leaves")
	}

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
