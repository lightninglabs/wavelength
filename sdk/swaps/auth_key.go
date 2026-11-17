package swaps

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightninglabs/lndclient"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/zpay32"
)

// invoiceRoutingKey signs and decodes Lightning onions for one invoice. It
// is safe to delegate to the receive service because it is independent of
// the wallet keys used to authorize or claim the Ark contract.
type invoiceRoutingKey struct {
	*keychain.PrivKeyMessageSigner
	*sphinx.PrivKeyECDH
}

// PubKey resolves the public key common to the signer and ECDH adapter.
func (k *invoiceRoutingKey) PubKey() *btcec.PublicKey {
	return k.PrivKey.PubKey()
}

// deriveInvoiceRoutingKey uses the wallet's existing receive-auth ECDH with
// a separate domain and a fixed public point. Only the resulting routing
// scalar is shared. The input wallet key and its legacy invoice key stay
// private, and the same seed can recover the routing key after restart.
func (c *SwapClient) deriveInvoiceRoutingKey(ctx context.Context,
	hash lntypes.Hash) (*invoiceRoutingKey, error) {

	input := chainhash.TaggedHash(
		[]byte("wavelength/swap/invoice-routing/v1"), hash[:],
	)
	secret, err := c.daemon.ReceiveAuthECDH(
		ctx, lntypes.Hash(*input), lndclient.SharedKeyNUMS,
	)
	if err != nil {
		return nil, fmt.Errorf("derive invoice routing key: %w", err)
	}

	key, _ := btcec.PrivKeyFromBytes(secret[:])
	if key.Key.IsZero() {
		return nil, fmt.Errorf("zero invoice routing key")
	}

	return &invoiceRoutingKey{
		PrivKeyMessageSigner: keychain.NewPrivKeyMessageSigner(
			key, keychain.KeyLocator{},
		),
		PrivKeyECDH: &sphinx.PrivKeyECDH{
			PrivKey: key,
		},
	}, nil
}

// receiveOnionKey selects the key committed by the stored BOLT-11 invoice.
// Older records without an invoice keep the legacy decoder, which still
// authenticates and checks the onion before the wallet accepts it.
func (s *ReceiveSession) receiveOnionKey(ctx context.Context) (ReceiveAuthKey,
	error) {

	legacy, err := s.client.receiveAuthKey(ctx, s.PaymentHash)
	if err != nil {
		return nil, err
	}
	if s.client.chainParams == nil || s.Invoice == "" {
		return legacy, nil
	}
	invoice, err := zpay32.Decode(s.Invoice, s.client.chainParams)
	if err != nil {
		return nil, fmt.Errorf("decode stored receive invoice: %w", err)
	}
	if invoice.Destination.IsEqual(legacy.PubKey()) {
		return legacy, nil
	}

	key, err := s.client.deriveInvoiceRoutingKey(ctx, s.PaymentHash)
	if err != nil {
		return nil, err
	}
	if !invoice.Destination.IsEqual(key.PubKey()) {
		return nil, fmt.Errorf("receive invoice destination does not " +
			"match wallet keys")
	}

	return key, nil
}

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
