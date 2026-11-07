package arkscript

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
)

// ComposedBoardingScript derives the deposit script from the disclosed asset
// root and boarding policy. Comparing it with the confirmed output binds both
// commitments without disclosing the asset tree's preimage. It returns the
// pkScript, internal key, and combined policy/asset merkle root.
func ComposedBoardingScript(policyTemplate []byte,
	commitmentLeafHash [32]byte) ([]byte, *btcec.PublicKey, [32]byte,
	error) {

	template, err := DecodePolicyTemplate(policyTemplate)
	if err != nil {
		return nil, nil, [32]byte{}, fmt.Errorf("decode boarding "+
			"policy: %w", err)
	}
	policy, err := template.Compile()
	if err != nil {
		return nil, nil, [32]byte{}, err
	}
	composed, err := ComposeWithSiblingRoot(
		policy, chainhash.Hash(commitmentLeafHash),
	)
	if err != nil {
		return nil, nil, [32]byte{}, err
	}
	script, err := txscript.PayToTaprootScript(composed.OutputKey())
	if err != nil {
		return nil, nil, [32]byte{}, err
	}

	root := [32]byte(composed.CombinedRoot)

	return script, composed.InternalKey, root, nil
}

// ComposedBoardingExitSpend returns the owner's exit spend of a deposit output
// that composes the standard boarding policy with an asset commitment leaf.
// The leaf hash becomes the top-level sibling in the exit leaf's control
// block, so the spend binds to the composed output rather than to the plain
// boarding output.
func ComposedBoardingExitSpend(ownerKey, operatorKey *btcec.PublicKey,
	exitDelay uint32, commitmentLeafHash [32]byte) (*SpendPath, error) {

	policy, err := NewVTXOPolicy(ownerKey, operatorKey, exitDelay)
	if err != nil {
		return nil, err
	}
	exit, err := policy.ExitSpendInfo()
	if err != nil {
		return nil, err
	}

	control, err := txscript.ParseControlBlock(exit.ControlBlock)
	if err != nil {
		return nil, fmt.Errorf("parse exit control block: %w", err)
	}
	policyRoot := control.RootHash(exit.WitnessScript)
	composedRoot := tapBranchHashCompose(
		chainhash.Hash(policyRoot), chainhash.Hash(commitmentLeafHash),
	)
	outputKey := txscript.ComputeTaprootOutputKey(
		control.InternalKey, composedRoot[:],
	)

	composed := *control
	composed.OutputKeyYIsOdd = outputKey.SerializeCompressed()[0] == 0x03
	composed.InclusionProof = append(
		bytes.Clone(control.InclusionProof), commitmentLeafHash[:]...,
	)
	controlBlock, err := composed.ToBytes()
	if err != nil {
		return nil, fmt.Errorf("encode exit control block: %w", err)
	}

	return &SpendPath{
		SpendInfo: &SpendInfo{
			WitnessScript: bytes.Clone(exit.WitnessScript),
			ControlBlock:  controlBlock,
		},
		RequiredSequence: exitDelay,
	}, nil
}
