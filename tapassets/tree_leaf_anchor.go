package tapassets

import (
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/lightninglabs/wavelength/lib/arkscript"
)

// StandardVTXOLeafAnchor returns the taproot data for a standard VTXO leaf.
func StandardVTXOLeafAnchor(owner, operator *btcec.PublicKey,
	exitDelay uint32) (TreeLeafAnchor, error) {

	encoded, err := arkscript.EncodeStandardVTXOTemplate(
		owner, operator, exitDelay,
	)
	if err != nil {
		return TreeLeafAnchor{}, fmt.Errorf("encode leaf policy: %w",
			err)
	}
	template, err := arkscript.DecodePolicyTemplate(encoded)
	if err != nil {
		return TreeLeafAnchor{}, fmt.Errorf("decode leaf policy: %w",
			err)
	}
	policy, err := template.Compile()
	if err != nil {
		return TreeLeafAnchor{}, fmt.Errorf("compile leaf policy: %w",
			err)
	}
	pkScript, err := template.PkScript()
	if err != nil {
		return TreeLeafAnchor{}, fmt.Errorf("leaf policy script: %w",
			err)
	}

	tapLeaves := make([]txscript.TapLeaf, len(policy.Leaves))
	for idx := range policy.Leaves {
		tapLeaves[idx] = policy.Leaves[idx].Leaf
	}

	return TreeLeafAnchor{
		UncomposedPkScript: pkScript,
		InternalKey:        policy.InternalKey,
		TapLeaves:          tapLeaves,
	}, nil
}
