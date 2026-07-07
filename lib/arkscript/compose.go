package arkscript

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
)

// ComposedPolicy represents a policy that combines an Ark policy root with
// an external root (e.g., from Taproot Assets).
type ComposedPolicy struct {
	// InternalKey is the (unspendable) internal key for this policy.
	InternalKey *btcec.PublicKey

	// PolicyRoot is the merkle root of the Ark policy subtree.
	PolicyRoot chainhash.Hash

	// ExternalRoot is the externally provided root to combine with.
	ExternalRoot chainhash.Hash

	// CombinedRoot is the final merkle root after composition.
	CombinedRoot chainhash.Hash

	// ArkPolicy is the underlying Ark policy for spend info retrieval.
	ArkPolicy *CompiledPolicy
}

// OutputKey computes the taproot output key for this composed policy.
func (c *ComposedPolicy) OutputKey() *btcec.PublicKey {
	return txscript.ComputeTaprootOutputKey(
		c.InternalKey, c.CombinedRoot[:],
	)
}

// SpendInfo returns the spend information for a leaf in the Ark policy
// subtree. The control block's merkle proof will include the external root
// as an additional sibling at the root level.
func (c *ComposedPolicy) SpendInfo(leafIndex int) (*SpendInfo, error) {
	// Get the base spend info from the Ark policy.
	info, err := c.ArkPolicy.SpendInfo(leafIndex)
	if err != nil {
		return nil, err
	}

	// Rebuild the control block with the composed root.
	// The original control block has:
	//   - 1 byte: control byte
	//   - 32 bytes: internal key
	//   - N * 32 bytes: merkle proof siblings
	//
	// For composed policies, we need to add the external root as an
	// additional sibling at the end of the proof.

	if len(info.ControlBlock) < 33 {
		return nil, fmt.Errorf("invalid control block length")
	}

	// Extract control byte and internal key.
	controlByte := info.ControlBlock[0]
	internalKeyBytes := info.ControlBlock[1:33]
	originalProof := info.ControlBlock[33:]

	// Recalculate control byte parity for the new output key.
	outputKey := c.OutputKey()
	outputKeyParity := outputKey.SerializeCompressed()[0] == 0x03

	newControlByte := controlByte & 0xfe // Clear parity bit
	if outputKeyParity {
		newControlByte |= 0x01
	}

	// Build new control block with external root appended.
	newControlBlock := make([]byte, 0, 33+len(originalProof)+32)
	newControlBlock = append(newControlBlock, newControlByte)
	newControlBlock = append(newControlBlock, internalKeyBytes...)
	newControlBlock = append(newControlBlock, originalProof...)
	newControlBlock = append(newControlBlock, c.ExternalRoot[:]...)

	return &SpendInfo{
		WitnessScript: info.WitnessScript,
		ControlBlock:  newControlBlock,
	}, nil
}

// ComposeWithSiblingRoot combines an Ark policy with an external root.
// The combined root is computed as:
//
//	TapBranchHash(min(policyRoot, extRoot), max(policyRoot, extRoot))
//
// The internal key must be unspendable (no key-path spend).
func ComposeWithSiblingRoot(policy *CompiledPolicy,
	externalRoot chainhash.Hash) (*ComposedPolicy, error) {

	if policy == nil {
		return nil, fmt.Errorf("compose: policy is nil")
	}

	// Get the policy root.
	var policyRoot chainhash.Hash
	copy(policyRoot[:], policy.RootHash)

	// Compute combined root: TapBranchHash(min, max).
	combinedRoot := tapBranchHashCompose(policyRoot, externalRoot)

	return &ComposedPolicy{
		InternalKey:  policy.InternalKey,
		PolicyRoot:   policyRoot,
		ExternalRoot: externalRoot,
		CombinedRoot: combinedRoot,
		ArkPolicy:    policy,
	}, nil
}

// tapBranchHashCompose computes the BIP-341 TapBranch hash for combining two
// roots.
func tapBranchHashCompose(a, b chainhash.Hash) chainhash.Hash {
	// Sort lexicographically per BIP-341.
	if bytes.Compare(a[:], b[:]) > 0 {
		a, b = b, a
	}

	return *chainhash.TaggedHash(chainhash.TagTapBranch, a[:], b[:])
}

// PolicyRoot returns just the policy root hash from a compiled policy.
// This can be used when the caller needs to provide the root for external
// composition. The policy must not be nil.
func PolicyRoot(policy *CompiledPolicy) chainhash.Hash {
	if policy == nil {
		return chainhash.Hash{}
	}

	var root chainhash.Hash
	copy(root[:], policy.RootHash)

	return root
}
