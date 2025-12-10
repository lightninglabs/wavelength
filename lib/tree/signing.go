package tree

import (
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
)

// Musig2PubNonce is a public nonce for MuSig2 signing.
type Musig2PubNonce [musig2.PubNonceSize]byte

// TxSignerSession manages signing for a single transaction on the client or
// operator side. It uses lnd's MuSig2Signer interface.
//
// SECURITY: Each TxSignerSession automatically generates fresh nonces via
// lnd's MuSig2 implementation. Do NOT reuse a TxSignerSession for signing
// multiple transactions or re-signing the same transaction, as this would
// constitute nonce reuse and leak the private key.
type TxSignerSession struct {
	signer      input.MuSig2Signer
	signSession *input.MuSig2SessionInfo
	sigHash     [32]byte
}

// NewTxSignerSession creates a new signing session for a single transaction.
func NewTxSignerSession(signer input.MuSig2Signer,
	sweepTapscriptRoot []byte, cosigners []*btcec.PublicKey,
	signerKey *keychain.KeyDescriptor, tx *wire.MsgTx,
	fetcher txscript.PrevOutputFetcher) (*TxSignerSession, error) {

	// Validate inputs.
	if signer == nil {
		return nil, fmt.Errorf("signer cannot be nil")
	}

	if signerKey == nil || signerKey.PubKey == nil {
		return nil, fmt.Errorf("signer key cannot be nil")
	}

	if len(cosigners) == 0 {
		return nil, fmt.Errorf("cosigners cannot be empty")
	}

	if tx == nil {
		return nil, fmt.Errorf("transaction cannot be nil")
	}

	// Calculate signature hash.
	message, err := txscript.CalcTaprootSignatureHash(
		txscript.NewTxSigHashes(tx, fetcher),
		txscript.SigHashDefault, tx, 0, fetcher,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to calculate signature hash: %w",
			err)
	}

	// Create MuSig2 session.
	musigSession, err := signer.MuSig2CreateSession(
		input.MuSig2Version100RC2, signerKey.KeyLocator,
		cosigners, &input.MuSig2Tweaks{
			TaprootTweak: sweepTapscriptRoot,
		}, nil, nil,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create MuSig2 session: %w",
			err)
	}

	return &TxSignerSession{
		signer:      signer,
		signSession: musigSession,
		sigHash:     [32]byte(message),
	}, nil
}

// GetNonce returns the public nonce for this signing session.
func (s *TxSignerSession) GetNonce() (Musig2PubNonce, error) {
	return s.signSession.PublicNonce, nil
}

// RegisterAggNonce registers the aggregated nonce for this signing session.
func (s *TxSignerSession) RegisterAggNonce(
	aggNonce [musig2.PubNonceSize]byte) error {

	return s.signer.MuSig2RegisterCombinedNonce(
		s.signSession.SessionID, aggNonce,
	)
}

// Sign generates the partial signature for this transaction.
// If cleanup is true, the signing session is cleaned up after signing.
func (s *TxSignerSession) Sign(cleanup bool) (*musig2.PartialSignature, error) {
	return s.signer.MuSig2Sign(
		s.signSession.SessionID, s.sigHash, cleanup,
	)
}

// SignerSession manages signing for all transactions in a client's path.
// It automatically extracts the signer's path and creates TxSignerSession for
// each transaction in that path.
type SignerSession struct {
	// signerKey is the key descriptor for the signer.
	signerKey *keychain.KeyDescriptor

	// txs maps transaction IDs to their signing sessions.
	txs map[string]*TxSignerSession
}

// NewSignerSession creates a new signing session for a tree. It
// automatically extracts the path for the given signer and creates sessions
// for each transaction in that path.
func NewSignerSession(signer input.MuSig2Signer,
	signerKey *keychain.KeyDescriptor, sweepTapscriptRoot []byte,
	prevOuts txscript.PrevOutputFetcher, tree *Node) (*SignerSession,
	error) {

	// Validate inputs.
	if signer == nil {
		return nil, fmt.Errorf("signer cannot be nil")
	}

	if signerKey == nil || signerKey.PubKey == nil {
		return nil, fmt.Errorf("signer key cannot be nil")
	}

	if tree == nil {
		return nil, fmt.Errorf("tree cannot be nil")
	}

	// Extract the path for this cosigner only.
	signerPath, ok := tree.ExtractPathForCoSigners(signerKey.PubKey)
	if !ok {
		return nil, fmt.Errorf("no path found for signer")
	}

	// Create signing sessions for each transaction in the path.
	txs := make(map[string]*TxSignerSession)

	err := signerPath.ForEach(func(node *Node) error {
		tx, err := node.ToTx()
		if err != nil {
			return fmt.Errorf("failed to create tx: %w", err)
		}

		session, err := NewTxSignerSession(
			signer, sweepTapscriptRoot, node.CoSigners,
			signerKey, tx, prevOuts,
		)
		if err != nil {
			return fmt.Errorf("failed to create tx session: %w",
				err)
		}

		txs[tx.TxHash().String()] = session

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &SignerSession{
		signerKey: signerKey,
		txs:       txs,
	}, nil
}

// PubKey returns the signer's public key.
func (s *SignerSession) PubKey() *btcec.PublicKey {
	return s.signerKey.PubKey
}

// GetNonces returns nonces for all transactions in the signer's path.
func (s *SignerSession) GetNonces() (
	map[string]Musig2PubNonce, error) {

	nonces := make(map[string]Musig2PubNonce, len(s.txs))
	for txid, txSession := range s.txs {
		nonce, err := txSession.GetNonce()
		if err != nil {
			return nil, fmt.Errorf("failed to get nonce for "+
				"tx %s: %w", txid, err)
		}

		nonces[txid] = nonce
	}

	return nonces, nil
}

// RegisterAggNonces registers the aggregated nonce for each transaction in the
// signer's path.
func (s *SignerSession) RegisterAggNonces(
	nonceSet map[string]Musig2PubNonce) error {

	for txid, txSession := range s.txs {
		nonce, ok := nonceSet[txid]
		if !ok {
			return fmt.Errorf("aggregated nonce for tx %s not "+
				"found", txid)
		}

		err := txSession.RegisterAggNonce(nonce)
		if err != nil {
			return fmt.Errorf("failed to register aggregated "+
				"nonce for tx %s: %w", txid, err)
		}
	}

	return nil
}

// Signatures generates partial signatures for all transactions in the signer's
// path. If cleanup is true, the signing sessions are cleaned up after signing.
func (s *SignerSession) Signatures(cleanup bool) (
	map[string]*musig2.PartialSignature, error) {

	sigs := make(map[string]*musig2.PartialSignature, len(s.txs))
	for txid, txSession := range s.txs {
		sig, err := txSession.Sign(cleanup)
		if err != nil {
			return nil, fmt.Errorf("failed to sign tx %s: %w",
				txid, err)
		}

		sigs[txid] = sig
	}

	return sigs, nil
}
