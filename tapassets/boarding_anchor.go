package tapassets

import (
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/lightninglabs/wavelength/lib/arkscript"
)

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

	template, err := arkscript.DecodePolicyTemplate(policyTemplate)
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

	composedRoot := tapBranch(
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

// tapBranch hashes a BIP-341 branch over two node hashes, sorting them
// lexicographically as consensus requires.
func tapBranch(left, right [32]byte) [32]byte {
	if string(right[:]) < string(left[:]) {
		left, right = right, left
	}

	return [32]byte(
		*chainhash.TaggedHash(
			chainhash.TagTapBranch, left[:], right[:],
		),
	)
}
