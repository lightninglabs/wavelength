package arkscript

import (
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/lightningnetwork/lnd/input"
)

// CheckpointPolicy defines the parameters for constructing an OOR checkpoint
// taproot tree.
//
// This is intentionally interface-first and minimal: it provides enough
// information to deterministically derive a checkpoint output pkScript, while
// allowing the underlying closure system to evolve later.
type CheckpointPolicy struct {
	// OperatorKey is the public key required by the operator-controlled
	// CSV unroll leaf.
	OperatorKey *btcec.PublicKey

	// CSVDelay is the relative timelock enforced by the
	// operator-controlled leaf.
	//
	// This is a raw BIP-68 sequence value interpreted by
	// OP_CHECKSEQUENCEVERIFY.
	CSVDelay uint32
}

// CheckpointTapScript constructs the tapscript for an OOR checkpoint output.
//
// The checkpoint tree for v0 is a simple two-leaf tree:
//
//   - an operator-controlled CSV unroll leaf (operator key + relative
//     timelock),
//   - a collaborative leaf between operator and VTXO owner (provided by the
//     caller as raw script bytes).
//
// For v0, the checkpoint output always uses the ARK NUMS internal key so
// there is no key-path spend and all spends go through one of the script
// leaves.
func CheckpointTapScript(policy CheckpointPolicy,
	ownerLeafScript []byte) (*waddrmgr.Tapscript, error) {

	switch {
	case policy.OperatorKey == nil:
		return nil, fmt.Errorf("operator key must be provided")

	case len(ownerLeafScript) == 0:
		return nil, fmt.Errorf("owner leaf script must be provided")
	}

	unrollLeaf, err := UnilateralCSVTimeoutTapLeaf(
		policy.OperatorKey, policy.CSVDelay,
	)
	if err != nil {
		return nil, fmt.Errorf("unable to construct unroll leaf: %w",
			err)
	}

	ownerLeaf := txscript.NewBaseTapLeaf(ownerLeafScript)

	tapscript := input.TapscriptFullTree(
		&ARKNUMSKey, unrollLeaf, ownerLeaf,
	)

	// Compute and set the root hash since TapscriptFullTree doesn't
	// populate it.
	tree := txscript.AssembleTaprootScriptTree(tapscript.Leaves...)
	rootHash := tree.RootNode.TapHash()
	tapscript.RootHash = rootHash[:]

	return tapscript, nil
}

// CheckpointPkScript returns the pkScript for a checkpoint output produced
// by CheckpointTapScript.
func CheckpointPkScript(policy CheckpointPolicy,
	ownerLeafScript []byte) ([]byte, error) {

	tapscript, err := CheckpointTapScript(policy, ownerLeafScript)
	if err != nil {
		return nil, err
	}

	tapKey, err := tapscript.TaprootKey()
	if err != nil {
		return nil, fmt.Errorf("unable to compute taproot key: %w", err)
	}

	return txscript.PayToTaprootScript(tapKey)
}
