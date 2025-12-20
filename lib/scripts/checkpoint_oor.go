package scripts

import (
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/txscript"
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
	// InternalKey is the internal taproot key used for the checkpoint
	// output.
	//
	// For v0 OOR transfers, we default to the unspendable ARK NUMS key to
	// avoid creating an unintentional key path spend.
	InternalKey *btcec.PublicKey

	// OperatorKey is the public key required by the operator-controlled CSV
	// unroll leaf.
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
//   - an owner-controlled collaborative leaf (provided by the caller as raw
//     script).
//
// This function does not validate that ownerLeafScript is "a correct Ark
// closure". That validation belongs in higher layers once the canonical closure
// system is in place (see the closures PRs). For now, this gives OOR primitives
// a deterministic way to bind checkpoint scripts.
func CheckpointTapScript(policy CheckpointPolicy,
	ownerLeafScript []byte) (*waddrmgr.Tapscript, error) {

	switch {
	case policy.OperatorKey == nil:
		return nil, fmt.Errorf("operator key must be provided")

	case len(ownerLeafScript) == 0:
		return nil, fmt.Errorf("owner leaf script must be provided")
	}

	internalKey := policy.InternalKey
	if internalKey == nil {
		// Defaulting to the ARK NUMS key ensures there is no key-path
		// spend, which forces all spends to go through one of the
		// script leaves.
		internalKey = &ARKNUMSKey
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
		internalKey, unrollLeaf, ownerLeaf,
	)

	// Compute and set the root hash since TapscriptFullTree doesn't
	// populate it.
	tree := txscript.AssembleTaprootScriptTree(tapscript.Leaves...)
	rootHash := tree.RootNode.TapHash()
	tapscript.RootHash = rootHash[:]

	return tapscript, nil
}

// CheckpointPkScript returns the pkScript for a checkpoint output produced by
// CheckpointTapScript.
//
// The caller should treat this as the canonical way to derive checkpoint output
// scripts for v0 OOR transfers so both client and server can validate and
// serialize checkpoint transactions deterministically.
func CheckpointPkScript(policy CheckpointPolicy,
	ownerLeafScript []byte) ([]byte, error) {

	tapscript, err := CheckpointTapScript(policy, ownerLeafScript)
	if err != nil {
		return nil, err
	}

	tapKey, err := tapscript.TaprootKey()
	if err != nil {
		return nil, fmt.Errorf("unable to compute taproot key: %w",
			err)
	}

	return txscript.PayToTaprootScript(tapKey)
}
