package swaps

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	swapsqlc "github.com/lightninglabs/darepo-client/sdk/swaps/sqlc"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/keychain"
)

const (
	receiveAuthKeyID  = "default"
	receiveAuthKeyLen = 32
)

// ReceiveAuthKey is the stable client-level key used to sign receive invoices
// and decrypt the forwarded final-hop onion for those invoices.
type ReceiveAuthKey interface {
	keychain.SingleKeyMessageSigner
	sphinx.SingleKeyECDH
}

// localReceiveAuthKey keeps the generated receive-auth key behind the signer
// and ECDH interfaces used by invoice creation and onion decoding.
type localReceiveAuthKey struct {
	privKey *btcec.PrivateKey
	signer  *keychain.PrivKeyMessageSigner
}

// newLocalReceiveAuthKey wraps a private key as a receive-auth key.
func newLocalReceiveAuthKey(privKey *btcec.PrivateKey) (ReceiveAuthKey,
	error) {

	if privKey == nil {
		return nil, fmt.Errorf("receive auth private key is required")
	}

	return &localReceiveAuthKey{
		privKey: privKey,
		signer: keychain.NewPrivKeyMessageSigner(
			privKey, keychain.KeyLocator{},
		),
	}, nil
}

// receiveAuthKeyFromBytes restores a receive-auth key from the serialized
// private key material stored in the swap database.
func receiveAuthKeyFromBytes(keyBytes []byte) (ReceiveAuthKey, error) {
	if len(keyBytes) != receiveAuthKeyLen {
		return nil, fmt.Errorf(
			"receive auth key must be %d bytes", receiveAuthKeyLen,
		)
	}

	privKey, _ := btcec.PrivKeyFromBytes(keyBytes)

	return newLocalReceiveAuthKey(privKey)
}

// PubKey returns the public key for the receive-auth key.
func (k *localReceiveAuthKey) PubKey() *btcec.PublicKey {
	return k.privKey.PubKey()
}

// KeyLocator returns the placeholder locator used by the local signer.
func (k *localReceiveAuthKey) KeyLocator() keychain.KeyLocator {
	return k.signer.KeyLocator()
}

// SignMessage signs one message with the receive-auth key.
func (k *localReceiveAuthKey) SignMessage(message []byte,
	doubleHash bool) (*ecdsa.Signature, error) {

	return k.signer.SignMessage(message, doubleHash)
}

// SignMessageCompact signs one message with the receive-auth key.
func (k *localReceiveAuthKey) SignMessageCompact(message []byte,
	doubleHash bool) ([]byte, error) {

	return k.signer.SignMessageCompact(message, doubleHash)
}

// ECDH derives the Sphinx shared secret with a remote onion ephemeral key.
func (k *localReceiveAuthKey) ECDH(pub *btcec.PublicKey) ([32]byte, error) {
	var pubJ btcec.JacobianPoint
	pub.AsJacobian(&pubJ)

	var ecdhPoint btcec.JacobianPoint
	btcec.ScalarMultNonConst(&k.privKey.Key, &pubJ, &ecdhPoint)

	ecdhPoint.ToAffine()
	ecdhPubKey := btcec.NewPublicKey(&ecdhPoint.X, &ecdhPoint.Y)

	return sha256.Sum256(ecdhPubKey.SerializeCompressed()), nil
}

// receiveAuthKey returns the stable receive-auth key for this client, creating
// and persisting one when the swap store does not have one yet.
func (c *SwapClient) receiveAuthKey(
	ctx context.Context) (ReceiveAuthKey, error) {

	c.receiveAuthMu.Lock()
	defer c.receiveAuthMu.Unlock()

	if c.receiveAuthKeyVal != nil {
		return c.receiveAuthKeyVal, nil
	}

	if c.store != nil && c.store.queries != nil {
		key, err := c.loadOrCreateReceiveAuthKey(ctx)
		if err != nil {
			return nil, err
		}

		c.receiveAuthKeyVal = key

		return key, nil
	}

	key, err := generateReceiveAuthKey()
	if err != nil {
		return nil, err
	}

	c.receiveAuthKeyVal = key

	return key, nil
}

// loadOrCreateReceiveAuthKey loads the durable receive-auth key or inserts a
// freshly generated one when the store has not been initialized with one.
func (c *SwapClient) loadOrCreateReceiveAuthKey(
	ctx context.Context) (ReceiveAuthKey, error) {

	keyBytes, err := c.store.queries.GetReceiveAuthKey(
		ctx, receiveAuthKeyID,
	)
	if err == nil {
		return receiveAuthKeyFromBytes(keyBytes)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("load receive auth key: %w", err)
	}

	privKey, err := btcec.NewPrivateKey()
	if err != nil {
		return nil, fmt.Errorf("generate receive auth key: %w", err)
	}

	keyBytes = privKey.Serialize()
	err = c.store.queries.InsertReceiveAuthKey(
		ctx, swapsqlc.InsertReceiveAuthKeyParams{
			KeyID:      receiveAuthKeyID,
			PrivateKey: keyBytes,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("persist receive auth key: %w", err)
	}

	storedBytes, err := c.store.queries.GetReceiveAuthKey(
		ctx, receiveAuthKeyID,
	)
	if err != nil {
		return nil, fmt.Errorf("reload receive auth key: %w", err)
	}

	return receiveAuthKeyFromBytes(storedBytes)
}

// generateReceiveAuthKey creates one in-memory fallback receive-auth key.
func generateReceiveAuthKey() (ReceiveAuthKey, error) {
	privKey, err := btcec.NewPrivateKey()
	if err != nil {
		return nil, fmt.Errorf("generate receive auth key: %w", err)
	}

	return newLocalReceiveAuthKey(privKey)
}
