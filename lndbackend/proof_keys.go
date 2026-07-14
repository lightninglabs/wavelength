package lndbackend

import (
	"context"
	"fmt"

	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/wavelength/indexer"
	"github.com/lightninglabs/wavelength/proofkeys"
	"github.com/lightningnetwork/lnd/keychain"
)

// ProofKeyBackend adapts lnd's wallet and signer RPCs to the shared proof key
// capability used by waved.
type ProofKeyBackend struct {
	walletKit lndclient.WalletKitClient
	signer    lndclient.SignerClient
}

// NewProofKeyBackend creates a proof-key backend backed by lnd RPCs.
func NewProofKeyBackend(walletKit lndclient.WalletKitClient,
	signer lndclient.SignerClient) *ProofKeyBackend {

	return &ProofKeyBackend{
		walletKit: walletKit,
		signer:    signer,
	}
}

// DeriveKey returns the stable key identified by loc via WalletKit.
func (b *ProofKeyBackend) DeriveKey(ctx context.Context,
	loc keychain.KeyLocator) (*keychain.KeyDescriptor, error) {

	keyDesc, err := b.walletKit.DeriveKey(ctx, &loc)
	if err != nil {
		return nil, fmt.Errorf("derive key: %w", err)
	}

	return keyDesc, nil
}

// DeriveNextKey derives the next key in family via WalletKit.
func (b *ProofKeyBackend) DeriveNextKey(ctx context.Context,
	family keychain.KeyFamily) (*keychain.KeyDescriptor, error) {

	keyDesc, err := b.walletKit.DeriveNextKey(ctx, int32(family))
	if err != nil {
		return nil, fmt.Errorf("derive next key: %w", err)
	}

	return keyDesc, nil
}

// ProofSigner returns an lnd-backed indexer proof signer for keyDesc.
func (b *ProofKeyBackend) ProofSigner(
	keyDesc keychain.KeyDescriptor) indexer.SchnorrSigner {

	return indexer.NewLNDSchnorrSigner(b.signer, keyDesc)
}

var _ proofkeys.Backend = (*ProofKeyBackend)(nil)
