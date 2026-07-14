package proofkeys

import (
	"context"

	"github.com/lightninglabs/wavelength/indexer"
	"github.com/lightningnetwork/lnd/keychain"
)

// Backend exposes the wallet-managed key operations needed for daemon-owned
// receive scripts and indexer proof generation across wallet backends.
type Backend interface {
	// DeriveKey returns the stable key identified by loc.
	DeriveKey(context.Context,
		keychain.KeyLocator) (*keychain.KeyDescriptor, error)

	// DeriveNextKey returns the next key in the given family.
	DeriveNextKey(context.Context,
		keychain.KeyFamily) (*keychain.KeyDescriptor, error)

	// ProofSigner returns a signer bound to keyDesc for indexer proofs.
	ProofSigner(keychain.KeyDescriptor) indexer.SchnorrSigner
}
