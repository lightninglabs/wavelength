package arkscript

import (
	"fmt"

	btcaddr "github.com/btcsuite/btcd/address/v2"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcwallet/waddrmgr"
)

// ComposedBoardingAddress derives the boarding material for a composed
// asset output: the taproot address the output pays to, and the
// script-spend tapscript whose collaborative leaf carries the disclosed
// commitment leaf hash as its extra sibling. Both the owner signing the
// round's boarding input and the operator validating it must reconstruct
// the identical composed root, so they share this derivation.
func ComposedBoardingAddress(policyTemplate []byte, commitmentLeafHash [32]byte,
	ownerKey, operatorKey *btcec.PublicKey, exitDelay uint32,
	params *chaincfg.Params) (btcaddr.Address, *waddrmgr.Tapscript, error) {

	_, internalKey, composedRoot, err := ComposedBoardingScript(
		policyTemplate, commitmentLeafHash,
	)
	if err != nil {
		return nil, nil, err
	}

	plain, err := VTXOTapScript(ownerKey, operatorKey, exitDelay)
	if err != nil {
		return nil, nil, fmt.Errorf("boarding tapscript: %w", err)
	}
	if plain.ControlBlock == nil || len(plain.Leaves) < 2 {
		return nil, nil, fmt.Errorf("boarding tapscript is incomplete")
	}

	outputKey := txscript.ComputeTaprootOutputKey(
		internalKey, composedRoot[:],
	)
	address, err := btcaddr.NewAddressTaproot(
		schnorr.SerializePubKey(outputKey), params,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("composed boarding address: %w",
			err)
	}

	// The collaborative leaf's inclusion proof gains the commitment
	// leaf hash as its top-level sibling, so witness assembly rebuilds
	// the composed root without knowing that leaf's preimage.
	collab, timeout := plain.Leaves[0], plain.Leaves[1]
	timeoutHash := txscript.NewTapLeaf(
		timeout.LeafVersion, timeout.Script,
	).TapHash()

	proof := make([]byte, 0, 64)
	proof = append(proof, timeoutHash[:]...)
	proof = append(proof, commitmentLeafHash[:]...)

	return address, &waddrmgr.Tapscript{
		Type:   plain.Type,
		Leaves: plain.Leaves,
		ControlBlock: &txscript.ControlBlock{
			InternalKey: internalKey,
			OutputKeyYIsOdd: outputKey.
				SerializeCompressed()[0] == 0x03,
			LeafVersion:    collab.LeafVersion,
			InclusionProof: proof,
		},
	}, nil
}

// ComposedBoardingScript recomputes a boarded asset output's script from
// hashes alone: the boarding policy's canonical tap tree root is branched
// with the disclosed asset commitment leaf hash, and the resulting root
// tweaks the policy's internal key. Byte equality with the on-chain
// script authenticates the disclosure — finding a different preimage set
// for the same output key would break taproot. The returned composed
// root doubles as the input's signing tweak, and the disclosed hash is
// the control-block sibling for the policy's script spends.
func ComposedBoardingScript(policyTemplate []byte,
	commitmentLeafHash [32]byte) ([]byte, *btcec.PublicKey, [32]byte,
	error) {

	template, err := DecodePolicyTemplate(policyTemplate)
	if err != nil {
		return nil, nil, [32]byte{},
			fmt.Errorf("decode boarding policy: %w", err)
	}
	policy, err := template.Compile()
	if err != nil {
		return nil, nil, [32]byte{},
			fmt.Errorf("compile boarding policy: %w", err)
	}
	if len(policy.RootHash) != 32 {
		return nil, nil, [32]byte{},
			fmt.Errorf("boarding policy root hash is %d bytes",
				len(policy.RootHash))
	}

	composedRoot := composedTapBranch(
		[32]byte(policy.RootHash), commitmentLeafHash,
	)
	outputKey := txscript.ComputeTaprootOutputKey(
		policy.InternalKey, composedRoot[:],
	)
	pkScript, err := txscript.PayToTaprootScript(outputKey)
	if err != nil {
		return nil, nil, [32]byte{},
			fmt.Errorf("composed boarding script: %w", err)
	}

	return pkScript, policy.InternalKey, composedRoot, nil
}

// composedTapBranch hashes a BIP-341 branch over two node hashes,
// sorting them lexicographically as consensus requires.
func composedTapBranch(left, right [32]byte) [32]byte {
	if string(right[:]) < string(left[:]) {
		left, right = right, left
	}

	return [32]byte(
		*chainhash.TaggedHash(
			chainhash.TagTapBranch, left[:], right[:],
		),
	)
}

// ComposeSpendInfoWithSibling extends a policy-only leaf's control block
// with one extra top-level sibling, so the leaf can be spent from a
// composed output whose tap tree branches the policy root with that
// sibling. The output-key parity is recomputed for the composed root.
func ComposeSpendInfoWithSibling(info *SpendInfo,
	sibling [32]byte) (*SpendInfo, error) {

	if info == nil {
		return nil, fmt.Errorf("spend info is required")
	}
	controlBlock, err := txscript.ParseControlBlock(info.ControlBlock)
	if err != nil {
		return nil, fmt.Errorf("parse control block: %w", err)
	}

	policyRoot := controlBlock.RootHash(info.WitnessScript)
	composedRoot := composedTapBranch([32]byte(policyRoot), sibling)
	outputKey := txscript.ComputeTaprootOutputKey(
		controlBlock.InternalKey, composedRoot[:],
	)

	composed := *controlBlock
	composed.OutputKeyYIsOdd = outputKey.SerializeCompressed()[0] == 0x03
	composed.InclusionProof = append(
		append(
			[]byte(nil), controlBlock.InclusionProof...,
		),
		sibling[:]...,
	)

	encoded, err := composed.ToBytes()
	if err != nil {
		return nil, fmt.Errorf("encode composed control block: %w", err)
	}

	return &SpendInfo{
		WitnessScript: append([]byte(nil), info.WitnessScript...),
		ControlBlock:  encoded,
	}, nil
}

// ComposedBoardingAuthSpend returns the unilateral-timeout spend path for
// a composed boarding output. Join-round authorization proves ownership
// over the timeout leaf, which on a composed output needs the disclosed
// commitment leaf hash as an additional control-block sibling.
func ComposedBoardingAuthSpend(commitmentLeafHash [32]byte,
	ownerKey, operatorKey *btcec.PublicKey, exitDelay uint32) (*SpendPath,
	error) {

	plain, err := NewVTXOSpendInfoFromPolicy(
		ownerKey, operatorKey, exitDelay, 1,
	)
	if err != nil {
		return nil, fmt.Errorf("boarding timeout spend info: %w", err)
	}

	composed, err := ComposeSpendInfoWithSibling(plain, commitmentLeafHash)
	if err != nil {
		return nil, err
	}

	return &SpendPath{SpendInfo: composed}, nil
}
