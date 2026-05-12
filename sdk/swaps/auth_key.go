package swaps

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntypes"
)

// ReceiveAuthKey signs receive invoices and decrypts the forwarded final-hop
// onion for one payment-scoped receive-auth key.
type ReceiveAuthKey interface {
	keychain.SingleKeyMessageSigner
	sphinx.SingleKeyECDH
}

// daemonReceiveAuthKey adapts daemon-backed receive-auth operations to the
// signer and ECDH interfaces used by invoice creation and onion decoding.
type daemonReceiveAuthKey struct {
	ctx         func() context.Context
	daemon      DaemonConn
	paymentHash lntypes.Hash
	pubKey      *btcec.PublicKey
}

// PubKey returns the public key for the receive-auth key.
func (k *daemonReceiveAuthKey) PubKey() *btcec.PublicKey {
	return k.pubKey
}

// KeyLocator returns the placeholder locator used by daemon-backed receive
// auth.
func (k *daemonReceiveAuthKey) KeyLocator() keychain.KeyLocator {
	return keychain.KeyLocator{}
}

// SignMessage signs one message with the receive-auth key.
func (k *daemonReceiveAuthKey) SignMessage(message []byte, doubleHash bool) (
	*ecdsa.Signature, error) {

	return k.daemon.SignReceiveAuthMessage(
		k.ctx(), k.paymentHash, message, doubleHash,
	)
}

// SignMessageCompact signs one message with the receive-auth key.
func (k *daemonReceiveAuthKey) SignMessageCompact(message []byte,
	doubleHash bool) ([]byte, error) {

	return k.daemon.SignReceiveAuthMessageCompact(
		k.ctx(), k.paymentHash, message, doubleHash,
	)
}

// ECDH derives the Sphinx shared secret with a remote onion ephemeral key.
func (k *daemonReceiveAuthKey) ECDH(pub *btcec.PublicKey) ([32]byte, error) {
	return k.daemon.ReceiveAuthECDH(k.ctx(), k.paymentHash, pub)
}

// receiveAuthKey returns a daemon-backed payment-scoped receive-auth key. The
// SDK receives only a public key and delegates signing/ECDH to the daemon.
func (c *SwapClient) receiveAuthKey(ctx context.Context,
	paymentHash lntypes.Hash) (ReceiveAuthKey, error) {

	if c.daemon == nil {
		return nil, fmt.Errorf("daemon connection is required to use " +
			"receive auth key")
	}

	pubKey, err := c.daemon.ReceiveAuthKey(ctx, paymentHash)
	if err != nil {
		return nil, fmt.Errorf("get receive auth key: %w", err)
	}

	return &daemonReceiveAuthKey{
		ctx:         func() context.Context { return ctx },
		daemon:      c.daemon,
		paymentHash: paymentHash,
		pubKey:      pubKey,
	}, nil
}
