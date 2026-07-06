package tree

import (
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
)

// LeafDescriptor is a generic descriptor for a leaf output in a transaction
// tree. It is agnostic to whether the leaf represents a VTXO or connector.
type LeafDescriptor struct {
	// PkScript is the public key script for the leaf output. This is
	// typically a taproot script that includes both keyspend and
	// scriptspend paths.
	PkScript []byte

	// Amount is the value of the leaf output in satoshis.
	Amount btcutil.Amount

	// CoSignerKey is the public key of the leaf owner who must participate
	// in signing this leaf's transaction along with the operator.
	CoSignerKey *btcec.PublicKey
}
