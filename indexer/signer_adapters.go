package indexer

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnrpc/signrpc"
)

// KeyRingSchnorrSigner adapts a keychain.SecretKeyRing to SchnorrSigner for a
// fixed key descriptor.
type KeyRingSchnorrSigner struct {
	keyRing keychain.SecretKeyRing
	keyDesc keychain.KeyDescriptor
}

// NewKeyRingSchnorrSigner creates a Schnorr signer backed by a local secret
// key ring.
func NewKeyRingSchnorrSigner(keyRing keychain.SecretKeyRing,
	keyDesc keychain.KeyDescriptor) *KeyRingSchnorrSigner {

	return &KeyRingSchnorrSigner{
		keyRing: keyRing,
		keyDesc: keyDesc,
	}
}

// SignSchnorr signs the provided 32-byte digest with the configured key.
func (s *KeyRingSchnorrSigner) SignSchnorr(
	_ []byte, hash [32]byte) ([]byte, error) {

	if s.keyRing == nil {
		return nil, fmt.Errorf("secret key ring not configured")
	}

	privKey, err := s.keyRing.DerivePrivKey(s.keyDesc)
	if err != nil {
		return nil, fmt.Errorf("derive private key: %w", err)
	}

	sig, err := schnorr.Sign(privKey, hash[:])
	if err != nil {
		return nil, fmt.Errorf("sign digest: %w", err)
	}

	return sig.Serialize(), nil
}

// ProofPubKey returns the configured owner pubkey.
func (s *KeyRingSchnorrSigner) ProofPubKey(
	_ []byte) (*btcec.PublicKey, error) {

	if s.keyDesc.PubKey == nil {
		return nil, fmt.Errorf("key descriptor pubkey not configured")
	}

	return s.keyDesc.PubKey, nil
}

// SignSchnorrMessage signs the canonical proof preimage using the key ring's
// tagged-hash Schnorr signing path.
func (s *KeyRingSchnorrSigner) SignSchnorrMessage(ctx context.Context,
	_ []byte, message []byte, tag []byte) ([]byte, error) {

	_ = ctx

	if s.keyRing == nil {
		return nil, fmt.Errorf("secret key ring not configured")
	}

	sig, err := s.keyRing.SignMessageSchnorr(
		s.keyDesc.KeyLocator, message, false, nil, tag,
	)
	if err != nil {
		return nil, fmt.Errorf("sign tagged message: %w", err)
	}

	return sig.Serialize(), nil
}

// LNDSchnorrSigner adapts lnd's signrpc client to SchnorrSigner for a
// fixed key descriptor. When TapTweak is set, signatures are produced
// with the tweaked key (taproot output key) rather than the raw
// internal key.
type LNDSchnorrSigner struct {
	signer  lndclient.SignerClient
	keyDesc keychain.KeyDescriptor

	// TapTweak is the tapscript merkle root used to tweak the
	// internal key into the taproot output key. When nil, the
	// raw internal key is used (keyspend-only P2TR). When set,
	// signatures verify against P2TR(internalKey, tapTweak).
	TapTweak []byte
}

// NewLNDSchnorrSigner creates a Schnorr signer backed by lnd signrpc.
func NewLNDSchnorrSigner(signer lndclient.SignerClient,
	keyDesc keychain.KeyDescriptor) *LNDSchnorrSigner {

	return &LNDSchnorrSigner{
		signer:  signer,
		keyDesc: keyDesc,
	}
}

// WithTapTweak returns a copy of the signer with the given tapscript
// merkle root tweak. The resulting signer produces signatures that
// verify against the tweaked taproot output key.
func (s *LNDSchnorrSigner) WithTapTweak(
	tweak []byte) *LNDSchnorrSigner {

	return &LNDSchnorrSigner{
		signer:   s.signer,
		keyDesc:  s.keyDesc,
		TapTweak: tweak,
	}
}

// SignSchnorr returns an error because lnd's signrpc does not expose a raw
// digest Schnorr signing RPC for arbitrary prehashed messages.
func (s *LNDSchnorrSigner) SignSchnorr(
	_ []byte, _ [32]byte) ([]byte, error) {

	return nil, fmt.Errorf("raw digest schnorr signing is not supported " +
		"by lnd signrpc")
}

// ProofPubKey returns the configured owner pubkey.
func (s *LNDSchnorrSigner) ProofPubKey(
	_ []byte) (*btcec.PublicKey, error) {

	if s.keyDesc.PubKey == nil {
		return nil, fmt.Errorf("key descriptor pubkey not configured")
	}

	return s.keyDesc.PubKey, nil
}

// SignSchnorrMessage signs the canonical proof preimage using lnd's tagged
// message Schnorr signing RPC.
func (s *LNDSchnorrSigner) SignSchnorrMessage(ctx context.Context,
	_ []byte, message []byte, tag []byte) ([]byte, error) {

	if s.signer == nil {
		return nil, fmt.Errorf("lnd signer client not configured")
	}

	sig, err := s.signer.SignMessage(
		ctx, message, s.keyDesc.KeyLocator,
		lndclient.SignSchnorr(s.TapTweak),
		withSchnorrTag(tag),
	)
	if err != nil {
		return nil, fmt.Errorf("sign tagged message via lnd: %w", err)
	}

	return sig, nil
}

// withSchnorrTag applies a BIP-340 tag to lnd's SignMessage request.
func withSchnorrTag(tag []byte) lndclient.SignMessageOption {
	return func(req *signrpc.SignMessageReq) {
		req.Tag = tag
	}
}

var _ SchnorrSigner = (*KeyRingSchnorrSigner)(nil)
var _ SchnorrSigner = (*LNDSchnorrSigner)(nil)
