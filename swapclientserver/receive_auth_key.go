//go:build swapruntime

package swapclientserver

import (
	"crypto/sha256"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/lightninglabs/darepo-client/sdk/swaps"
	"github.com/lightningnetwork/lnd/keychain"
)

// daemonReceiveAuthKey adapts the daemon-local receive private key to the
// receive-auth interfaces used by sdk/swaps for invoice signing and onion
// decryption.
type daemonReceiveAuthKey struct {
	privKey *btcec.PrivateKey
	signer  *keychain.PrivKeyMessageSigner
}

// newDaemonReceiveAuthKey wraps a daemon-local private key as the receive-auth
// key that sdk/swaps expects for receive-swap invoices and forwarded onions.
func newDaemonReceiveAuthKey(
	privKey *btcec.PrivateKey) (swaps.ReceiveAuthKey, error) {

	if privKey == nil {
		return nil, fmt.Errorf("receive auth private key is required")
	}

	return &daemonReceiveAuthKey{
		privKey: privKey,
		signer: keychain.NewPrivKeyMessageSigner(
			privKey, keychain.KeyLocator{},
		),
	}, nil
}

// PubKey returns the public key for the daemon receive-auth key.
func (k *daemonReceiveAuthKey) PubKey() *btcec.PublicKey {
	return k.privKey.PubKey()
}

// KeyLocator returns the placeholder locator used by the local signer.
func (k *daemonReceiveAuthKey) KeyLocator() keychain.KeyLocator {
	return k.signer.KeyLocator()
}

// SignMessage signs a message with the daemon receive-auth key.
func (k *daemonReceiveAuthKey) SignMessage(message []byte,
	doubleHash bool) (*ecdsa.Signature, error) {

	return k.signer.SignMessage(message, doubleHash)
}

// SignMessageCompact signs a compact message with the daemon receive-auth key.
func (k *daemonReceiveAuthKey) SignMessageCompact(message []byte,
	doubleHash bool) ([]byte, error) {

	return k.signer.SignMessageCompact(message, doubleHash)
}

// ECDH derives the Sphinx shared secret with a remote onion ephemeral key.
func (k *daemonReceiveAuthKey) ECDH(pub *btcec.PublicKey) ([32]byte, error) {
	var pubJ btcec.JacobianPoint
	pub.AsJacobian(&pubJ)

	var ecdhPoint btcec.JacobianPoint
	btcec.ScalarMultNonConst(&k.privKey.Key, &pubJ, &ecdhPoint)

	ecdhPoint.ToAffine()
	ecdhPubKey := btcec.NewPublicKey(&ecdhPoint.X, &ecdhPoint.Y)

	return sha256.Sum256(ecdhPubKey.SerializeCompressed()), nil
}
