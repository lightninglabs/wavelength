package arkscript

import (
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
)

// ComposedBoardingScript derives the deposit script from the disclosed asset
// root and boarding policy. Comparing it with the confirmed output binds both
// commitments without disclosing the asset tree's preimage.
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
