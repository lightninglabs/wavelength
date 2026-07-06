package testutils

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/stretchr/testify/require"
)

// CreateKey returns a deterministically generated key pair. It returns the
// public key and mock signer for the key.
func CreateKey(index int32) (*btcec.PublicKey, input.Signer) {
	// Avoid all zeros, because it results in an invalid key.
	privKey, pubKey := btcec.PrivKeyFromBytes([]byte{
		0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
		0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, byte(index + 1),
	})

	mockSigner := input.NewMockSigner([]*btcec.PrivateKey{privKey}, nil)

	return pubKey, mockSigner
}

// TestSchnorrSignature creates a deterministic schnorr signature for tests.
// The seed string is hashed to create a private key, which signs a fixed test
// message.
func TestSchnorrSignature(t *testing.T, seed string) *schnorr.Signature {
	t.Helper()

	// Create a deterministic private key from the seed.
	h := chainhash.HashH([]byte(seed))
	privKey, _ := btcec.PrivKeyFromBytes(h[:])

	// Sign a test message.
	msg := chainhash.HashH([]byte("test message"))
	sig, err := schnorr.Sign(privKey, msg[:])
	require.NoError(t, err)

	return sig
}
