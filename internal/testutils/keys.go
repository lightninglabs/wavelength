package testutils

import (
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/input"
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
