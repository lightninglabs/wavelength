package assets

import (
	"encoding/hex"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
)

// MuSig2Signer represents one party in an N-of-N MuSig2 signing session. Each
// signer only knows their own private key and the other parties' public keys.
type MuSig2Signer struct {
	// privateKey is this signer's private key (kept secret).
	privateKey *btcec.PrivateKey

	// publicKey is this signer's public key.
	publicKey *btcec.PublicKey

	// cosignerPublicKeys are the public keys of all other cosigners.
	cosignerPublicKeys []*btcec.PublicKey

	// allPublicKeys contains all participants' public keys (this signer +
	// cosigners).
	allPublicKeys []*btcec.PublicKey

	// nonces contains the secret and public nonces for this signing
	// session.
	nonces *musig2.Nonces

	// cosignerNonces maps each cosigner's public key (hex) to their public
	// nonce. Nonces are received during the exchange phase.
	cosignerNonces map[string][66]byte

	// combinedNonce is the aggregated nonce from all parties. This is
	// computed once all cosigner nonces are received.
	combinedNonce *[66]byte

	// taprootTweak is the taproot tweak to apply (optional).
	taprootTweak []byte
}

// NewMuSig2Signer creates a new MuSig2 signer for one party in an N-of-N
// signing session.
//
// Parameters:
//   - privateKey: This party's private key.
//   - cosignerPublicKeys: The public keys of all other N-1 cosigners.
//   - taprootTweak: Optional taproot tweak (can be nil).
func NewMuSig2Signer(privateKey *btcec.PrivateKey,
	cosignerPublicKeys []*btcec.PublicKey, taprootTweak []byte) (
	*MuSig2Signer, error) {

	if len(cosignerPublicKeys) == 0 {
		return nil, fmt.Errorf("at least one cosigner public key " +
			"required")
	}

	// Generate nonces for this signing session.
	nonces, err := musig2.GenNonces(
		musig2.WithPublicKey(privateKey.PubKey()),
	)
	if err != nil {
		return nil, fmt.Errorf("generate nonces: %w", err)
	}

	// Build complete list of all participants' public keys.
	allPublicKeys := make([]*btcec.PublicKey, 0, len(cosignerPublicKeys)+1)
	allPublicKeys = append(allPublicKeys, privateKey.PubKey())
	allPublicKeys = append(allPublicKeys, cosignerPublicKeys...)

	return &MuSig2Signer{
		privateKey:         privateKey,
		publicKey:          privateKey.PubKey(),
		cosignerPublicKeys: cosignerPublicKeys,
		allPublicKeys:      allPublicKeys,
		nonces:             nonces,
		cosignerNonces:     make(map[string][66]byte),
		taprootTweak:       taprootTweak,
	}, nil
}

// PublicNonce returns this signer's public nonce to be sent to all other
// parties.
func (s *MuSig2Signer) PublicNonce() [66]byte {
	return s.nonces.PubNonce
}

// ReceiveNonce receives and stores a cosigner's public nonce. This must be
// called for each cosigner before signing. Once all cosigner nonces are
// received, the combined nonce is computed automatically.
func (s *MuSig2Signer) ReceiveNonce(cosignerPubKey *btcec.PublicKey,
	nonce [66]byte) error {

	pubKeyHex := hex.EncodeToString(schnorr.SerializePubKey(cosignerPubKey))

	// Check if nonce already received from this cosigner.
	if _, exists := s.cosignerNonces[pubKeyHex]; exists {
		return fmt.Errorf("nonce already received from cosigner %s",
			pubKeyHex[:8])
	}

	// Store the nonce.
	s.cosignerNonces[pubKeyHex] = nonce

	// Check if we have all nonces now.
	if len(s.cosignerNonces) == len(s.cosignerPublicKeys) {
		// All nonces received, compute combined nonce.
		if err := s.computeCombinedNonce(); err != nil {
			return fmt.Errorf("compute combined nonce: %w", err)
		}
	}

	return nil
}

// computeCombinedNonce aggregates all public nonces once they are all received.
func (s *MuSig2Signer) computeCombinedNonce() error {
	// Build list of all nonces (this signer + cosigners).
	allNonces := make([][66]byte, 0, len(s.cosignerPublicKeys)+1)
	allNonces = append(allNonces, s.nonces.PubNonce)

	// Add cosigner nonces in the same order as public keys for determinism.
	for _, pubKey := range s.cosignerPublicKeys {
		pubKeyHex := hex.EncodeToString(schnorr.SerializePubKey(pubKey))
		nonce, ok := s.cosignerNonces[pubKeyHex]
		if !ok {
			return fmt.Errorf("missing nonce from cosigner %s",
				pubKeyHex[:8])
		}

		allNonces = append(allNonces, nonce)
	}

	// Aggregate all nonces.
	combinedNonce, err := musig2.AggregateNonces(allNonces)
	if err != nil {
		return fmt.Errorf("aggregate nonces: %w", err)
	}

	s.combinedNonce = &combinedNonce

	return nil
}

// HaveAllNonces returns true if all cosigner nonces have been received.
func (s *MuSig2Signer) HaveAllNonces() bool {
	return len(s.cosignerNonces) == len(s.cosignerPublicKeys) &&
		s.combinedNonce != nil
}

// Sign creates a partial signature for the given message. All cosigner nonces
// must have been received first via ReceiveNonce.
func (s *MuSig2Signer) Sign(sigHash [32]byte) (*musig2.PartialSignature,
	error) {

	if !s.HaveAllNonces() {
		return nil, fmt.Errorf("must receive all %d cosigner nonces "+
			"first (have %d)", len(s.cosignerPublicKeys),
			len(s.cosignerNonces))
	}

	// Build sign options.
	signOpts := []musig2.SignOption{
		musig2.WithSortedKeys(),
		musig2.WithFastSign(),
	}

	// Add taproot tweak if present.
	if s.taprootTweak != nil {
		signOpts = append(signOpts,
			musig2.WithTaprootSignTweak(s.taprootTweak),
		)
	}

	// Create partial signature.
	partialSig, err := musig2.Sign(
		s.nonces.SecNonce, s.privateKey, *s.combinedNonce,
		s.allPublicKeys, sigHash, signOpts...,
	)
	if err != nil {
		return nil, fmt.Errorf("create partial signature: %w", err)
	}

	return partialSig, nil
}

// CombineSignatures combines all partial signatures to create the final Schnorr
// signature. This can be called by any party - all will produce the same final
// signature.
//
// Parameters:
//   - sigHash: The message hash that was signed.
//   - allPartialSigs: All N partial signatures (including this signer's).
func (s *MuSig2Signer) CombineSignatures(sigHash [32]byte,
	allPartialSigs []*musig2.PartialSignature) (*schnorr.Signature, error) {

	if !s.HaveAllNonces() {
		return nil, fmt.Errorf("must receive all cosigner nonces first")
	}

	// Validate we have the correct number of signatures.
	expectedSigCount := len(s.allPublicKeys)
	if len(allPartialSigs) != expectedSigCount {
		return nil, fmt.Errorf("expected %d partial signatures, got %d",
			expectedSigCount, len(allPartialSigs))
	}

	// Build combine options.
	combineOpts := []musig2.CombineOption{}

	// Add taproot tweak if present.
	if s.taprootTweak != nil {
		const sort = true
		combineOpts = append(combineOpts,
			musig2.WithTaprootTweakedCombine(
				sigHash, s.allPublicKeys, s.taprootTweak, sort,
			),
		)
	}

	// Use the R point from the first partial signature (all should have the
	// same R).
	rPoint := allPartialSigs[0].R

	// Combine partial signatures.
	finalSig := musig2.CombineSigs(rPoint, allPartialSigs, combineOpts...)

	return finalSig, nil
}
