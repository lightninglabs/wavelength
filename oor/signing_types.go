package oor

import (
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/wire"
)

// VTXOSigningDescriptor describes the minimum information needed for the
// operator to co-sign a checkpoint transaction spending a VTXO.
//
// The operator signature is produced for the standard collaborative VTXO leaf
// in the VTXO tapscript tree.
type VTXOSigningDescriptor struct {
	// Outpoint identifies the VTXO being spent by a checkpoint tx.
	Outpoint wire.OutPoint

	// OwnerKey is the public key of the VTXO owner.
	OwnerKey *btcec.PublicKey

	// ExitDelay is the VTXO unilateral exit delay used to derive the
	// timeout leaf.
	ExitDelay uint32
}
