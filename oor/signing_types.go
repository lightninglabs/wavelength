package oor

import "github.com/btcsuite/btcd/wire"

// VTXOSigningDescriptor describes the minimum semantic information needed for
// the operator to co-sign a checkpoint transaction spending a VTXO.
type VTXOSigningDescriptor struct {
	// Outpoint identifies the VTXO being spent by a checkpoint tx.
	Outpoint wire.OutPoint

	// VTXOPolicyTemplate is the serialized arkscript policy for the spent
	// input VTXO.
	VTXOPolicyTemplate []byte

	// SpendPath is the serialized arkscript spend path selected for the
	// checkpoint spend of the input VTXO.
	SpendPath []byte

	// OwnerLeafPolicy is the serialized arkscript owner-leaf policy for the
	// checkpoint output created from this input.
	OwnerLeafPolicy []byte
}
