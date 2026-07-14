package waved

import (
	"context"
	"fmt"

	"github.com/lightninglabs/wavelength/indexer"
	"github.com/lightningnetwork/lnd/keychain"
)

// indexerProofSignerFactory returns the signer factory for the active proof
// key backend.
func (s *Server) indexerProofSignerFactory() (OORReceiveScriptSignerFactory,
	error) {

	if s.proofKeyBackend == nil {
		return nil, fmt.Errorf("wallet backend not initialized")
	}

	return s.proofKeyBackend.ProofSigner, nil
}

// IndexerProofKey derives the fixed wallet key identified by loc and returns a
// signer that can produce indexer proof signatures for that key under the
// active wallet backend.
func (s *Server) IndexerProofKey(ctx context.Context, loc keychain.KeyLocator) (
	*keychain.KeyDescriptor, indexer.SchnorrSigner, error) {

	if s.proofKeyBackend == nil {
		return nil, nil, fmt.Errorf("wallet backend not initialized")
	}

	keyDesc, err := s.proofKeyBackend.DeriveKey(ctx, loc)
	if err != nil {
		return nil, nil, err
	}

	return keyDesc, s.proofKeyBackend.ProofSigner(*keyDesc), nil
}

// indexerProofNextKeyOps returns fresh-key derivation and signer-factory hooks
// for the active wallet backend.
func (s *Server) indexerProofNextKeyOps() (DeriveDefaultOORReceiveKeyFunc,
	OORReceiveScriptSignerFactory, error) {

	if s.proofKeyBackend == nil {
		return nil, nil, fmt.Errorf("wallet backend not initialized")
	}

	deriveNextKey := func(ctx context.Context) (*keychain.KeyDescriptor,
		error) {

		return s.proofKeyBackend.DeriveNextKey(
			ctx, oorReceiveKeyFamily,
		)
	}

	return deriveNextKey, s.proofKeyBackend.ProofSigner, nil
}
