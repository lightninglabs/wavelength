package assets

import (
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	tapasset "github.com/lightninglabs/taproot-assets/asset"
	"github.com/lightninglabs/taproot-assets/commitment"
)

// OpTrueArtifacts contains all components needed for OP_TRUE spending.
type OpTrueArtifacts struct {
	// SiblingPreimage is used when constructing the anchor output.
	SiblingPreimage *commitment.TapscriptPreimage

	// Witness is the witness stack for spending: [OP_TRUE, control_block].
	Witness wire.TxWitness

	// OutputKey is the tweaked taproot output key.
	OutputKey *btcec.PublicKey

	// TapLeaf is the tapscript leaf structure.
	TapLeaf *txscript.TapLeaf

	// ControlBlock is the control block for spending.
	ControlBlock *txscript.ControlBlock
}

// GetOpTrueScript returns a script that always evaluates to true.
func GetOpTrueScript() ([]byte, error) {
	return txscript.NewScriptBuilder().AddOp(txscript.OP_TRUE).Script()
}

// BuildOpTrueArtifacts creates OP_TRUE tapscript artifacts for simple spending.
//
// This function builds the minimal tapscript structure where the asset can be
// spent by simply revealing an OP_TRUE script. All spending control is enforced
// on the Bitcoin layer, not the asset layer.
//
// The internal key is always NUMS (Nothing Up My Sleeve) since the asset script
// is virtual and always spent via script path. The anchor output's spending
// path is controlled independently via the anchor internal key.
//
// Returns:
//   - SiblingPreimage: Used when constructing the anchor output's VOutput
//   - Witness: The witness stack for spending ([OP_TRUE_script, control_block])
//   - OutputKey: The tweaked taproot output key
//   - TapLeaf: The tap leaf structure
//   - ControlBlock: The control block for spending
func BuildOpTrueArtifacts() (*OpTrueArtifacts, error) {
	// Always use NUMS for the asset script internal key. This is a virtual
	// construct - the anchor spending path is separate.
	internalKey := tapasset.NUMSPubKey

	// Create the taproot OP_TRUE script using script builder
	tapScript, err := GetOpTrueScript()
	if err != nil {
		return nil, fmt.Errorf("create OP_TRUE script: %w", err)
	}

	// Create tap leaf from the OP_TRUE script
	tapLeaf := txscript.NewBaseTapLeaf(tapScript)

	// Create tapscript sibling preimage
	// This is used in VOutput.AnchorOutputTapscriptSibling
	siblingPreimage, err := commitment.NewPreimageFromLeaf(tapLeaf)
	if err != nil {
		return nil, fmt.Errorf("create sibling preimage: %w", err)
	}

	// Assemble the taproot script tree (just one leaf in our case)
	tapTree := txscript.AssembleTaprootScriptTree(tapLeaf)
	rootHash := tapTree.RootNode.TapHash()

	// Compute the tweaked output key:
	// internal_key + hash(internal_key || root_hash)
	outputKey := txscript.ComputeTaprootOutputKey(internalKey, rootHash[:])

	// Build control block for spending
	// The control block proves the script is part of the taproot tree
	controlBlock := &txscript.ControlBlock{
		LeafVersion: txscript.BaseLeafVersion,
		InternalKey: internalKey,
	}

	// Set the Y-coordinate parity bit based on the computed output key.
	// Check if Y coordinate is odd using the compressed format.
	outputKeyBytes := outputKey.SerializeCompressed()
	if outputKeyBytes[0] == secp256k1.PubKeyFormatCompressedOdd {
		controlBlock.OutputKeyYIsOdd = true
	}

	// Serialize the control block for the witness stack
	controlBlockBytes, err := controlBlock.ToBytes()
	if err != nil {
		return nil, fmt.Errorf("serialize control block: %w", err)
	}

	// Build the witness stack for spending. When spending, this proves:
	// "this script is in the tree, and it evaluates to true".
	witness := wire.TxWitness{tapScript, controlBlockBytes}

	return &OpTrueArtifacts{
		SiblingPreimage: siblingPreimage,
		Witness:         witness,
		OutputKey:       outputKey,
		TapLeaf:         &tapLeaf,
		ControlBlock:    controlBlock,
	}, nil
}
