package batch

import (
	"github.com/lightningnetwork/lnd/keychain"
)

// Terms encapsulates the various parameters and conditions that define a batch.
type Terms struct {
	// OperatorKey is the key descriptor for the operator's identity key.
	// This is the key that will be used as the signer in the musig2
	// signing sessions.
	OperatorKey keychain.KeyDescriptor

	// SweepKey is the public key used in the sweep path of VTXO trees.
	SweepKey keychain.KeyDescriptor

	// SweepDelay is the CSV delay for the sweep path in VTXO trees.
	SweepDelay uint32

	// MaxVTXOsPerTree is the maximum number of VTXOs in a single tree.
	MaxVTXOsPerTree uint32

	// TreeRadix is the branching factor for VTXO trees.
	TreeRadix uint32
}
