package waved

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightninglabs/wavelength/indexer"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

type stubProofKeyBackend struct {
	deriveKeyResp     *keychain.KeyDescriptor
	deriveKeyErr      error
	deriveKeyLoc      keychain.KeyLocator
	deriveNextKeyResp *keychain.KeyDescriptor
	deriveNextKeyErr  error
	deriveNextFamily  keychain.KeyFamily
	signerKeyDesc     keychain.KeyDescriptor
}

func (b *stubProofKeyBackend) DeriveKey(_ context.Context,
	loc keychain.KeyLocator) (*keychain.KeyDescriptor, error) {

	b.deriveKeyLoc = loc

	return b.deriveKeyResp, b.deriveKeyErr
}

func (b *stubProofKeyBackend) DeriveNextKey(_ context.Context,
	family keychain.KeyFamily) (*keychain.KeyDescriptor, error) {

	b.deriveNextFamily = family

	return b.deriveNextKeyResp, b.deriveNextKeyErr
}

func (b *stubProofKeyBackend) ProofSigner(
	keyDesc keychain.KeyDescriptor) indexer.SchnorrSigner {

	b.signerKeyDesc = keyDesc

	return indexer.NewKeyRingSchnorrSigner(nil, keyDesc)
}

func TestIndexerProofKey(t *testing.T) {
	t.Parallel()

	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	loc := keychain.KeyLocator{
		Family: oorReceiveKeyFamily,
		Index:  7,
	}
	wantDesc := &keychain.KeyDescriptor{
		KeyLocator: loc,
		PubKey:     privKey.PubKey(),
	}
	backend := &stubProofKeyBackend{
		deriveKeyResp: wantDesc,
	}
	server := &Server{
		proofKeyBackend: backend,
	}

	gotDesc, signer, err := server.IndexerProofKey(t.Context(), loc)
	require.NoError(t, err)
	require.Equal(t, loc, backend.deriveKeyLoc)
	require.Equal(t, wantDesc, gotDesc)
	require.Equal(t, *wantDesc, backend.signerKeyDesc)

	pubKeySource, ok := signer.(interface {
		ProofPubKey([]byte) (*btcec.PublicKey, error)
	})
	require.True(t, ok)

	pubKey, err := pubKeySource.ProofPubKey(nil)
	require.NoError(t, err)
	require.True(t, pubKey.IsEqual(privKey.PubKey()))
}

func TestIndexerProofNextKeyOps(t *testing.T) {
	t.Parallel()

	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	wantDesc := &keychain.KeyDescriptor{
		KeyLocator: keychain.KeyLocator{
			Family: oorReceiveKeyFamily,
			Index:  11,
		},
		PubKey: privKey.PubKey(),
	}
	backend := &stubProofKeyBackend{
		deriveNextKeyResp: wantDesc,
	}
	server := &Server{
		proofKeyBackend: backend,
	}

	deriveNext, signerFactory, err := server.indexerProofNextKeyOps()
	require.NoError(t, err)

	gotDesc, err := deriveNext(t.Context())
	require.NoError(t, err)
	require.Equal(t, oorReceiveKeyFamily, backend.deriveNextFamily)
	require.Equal(t, wantDesc, gotDesc)

	signer := signerFactory(*wantDesc)
	require.Equal(t, *wantDesc, backend.signerKeyDesc)

	pubKeySource, ok := signer.(interface {
		ProofPubKey([]byte) (*btcec.PublicKey, error)
	})
	require.True(t, ok)

	pubKey, err := pubKeySource.ProofPubKey(nil)
	require.NoError(t, err)
	require.True(t, pubKey.IsEqual(privKey.PubKey()))
}
