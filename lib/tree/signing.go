package tree

import (
	"errors"
	"fmt"
	"sync"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
)

// TxID is a type alias for chainhash.Hash, used as a key for maps that index
// by transaction ID. Using the raw hash type instead of strings improves type
// safety and avoids unnecessary string conversions.
type TxID = chainhash.Hash

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

	// cleanupMu serializes signing and cleanup so that a session cannot be
	// removed while it is being used. It also makes Cleanup idempotent.
	cleanupMu   sync.Mutex
	cleanupDone bool
}

// NewTxSignerSession creates a new signing session for a single transaction
// in a virtual transaction tree. The point of the session is to facilitate
// the MuSig2 signing process for a specific transaction. Each transaction has
// signs the key-spend path of the previous transaction. The parameters are:
//
//   - signer: The MuSig2Signer interface to use creating the session and
//     signing.
//   - sweepTapscriptRoot: The tapscript root used for tweaking the keyspend
//     path.
//   - signerKey: The key descriptor of the signer.
//   - fetcher: The PrevOutputFetcher to retrieve the output being spent by this
//     node's transaction.
func (n *Node) NewTxSignerSession(signer input.MuSig2Signer,
	sweepTapscriptRoot []byte, signerKey *keychain.KeyDescriptor,
	fetcher txscript.PrevOutputFetcher) (*TxSignerSession, error) {

	// Validate inputs.
	if signer == nil {
		return nil, fmt.Errorf("signer cannot be nil")
	}

	if signerKey == nil || signerKey.PubKey == nil {
		return nil, fmt.Errorf("signer key cannot be nil")
	}

	// Calculate signature hash.
	sigHash, err := n.SigHash(fetcher)
	if err != nil {
		return nil, fmt.Errorf("failed to calculate sig hash: %w", err)
	}

	// Create MuSig2 session for signing the transaction input via the
	// keyspend path.
	musigSession, err := n.NewSignerSession(
		signerKey, signer, sweepTapscriptRoot,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create MuSig2 session: %w",
			err)
	}

	return &TxSignerSession{
		signer:      signer,
		signSession: musigSession,
		sigHash:     [32]byte(sigHash),
	}, nil
}

// GetNonce returns the public nonce for this signing session.
func (s *TxSignerSession) GetNonce() Musig2PubNonce {
	return s.signSession.PublicNonce
}

// RegisterAggNonce registers the aggregated nonce for this signing session.
func (s *TxSignerSession) RegisterAggNonce(
	aggNonce [musig2.PubNonceSize]byte) error {

	s.cleanupMu.Lock()
	defer s.cleanupMu.Unlock()

	if s.cleanupDone {
		return fmt.Errorf("signing session has been cleaned up")
	}

	return s.signer.MuSig2RegisterCombinedNonce(
		s.signSession.SessionID, aggNonce,
	)
}

// Sign generates the partial signature for this transaction.
// If cleanup is true, the signing session is cleaned up after signing.
func (s *TxSignerSession) Sign(cleanup bool) (*musig2.PartialSignature, error) {
	s.cleanupMu.Lock()
	defer s.cleanupMu.Unlock()

	if s.cleanupDone {
		return nil, fmt.Errorf("signing session has been cleaned up")
	}

	sig, err := s.signer.MuSig2Sign(
		s.signSession.SessionID, s.sigHash, cleanup,
	)
	if err != nil {
		return nil, err
	}

	// MuSig2Sign removes the underlying session after a successful call
	// when cleanup is requested. Remember that here so a later Cleanup
	// call does not try to remove it a second time.
	if cleanup {
		s.cleanupDone = true
	}

	return sig, nil
}

// Cleanup removes the underlying MuSig2 session. It is safe to call Cleanup
// more than once after it succeeds. A failed cleanup remains retryable because
// a remote signer may recover from a transient transport error.
func (s *TxSignerSession) Cleanup() error {
	s.cleanupMu.Lock()
	defer s.cleanupMu.Unlock()

	if s.cleanupDone {
		return nil
	}

	err := s.signer.MuSig2Cleanup(s.signSession.SessionID)
	if err != nil {
		return err
	}

	s.cleanupDone = true

	return nil
}

// SignerSession manages signing for all transactions in a client's path in a
// tree for a given signing key. It automatically extracts the signer's path
// and creates TxSignerSession for each transaction in that path. If a client
// has multiple vtxo's in the tree, they will have a SignerSession for each
// vtxo's signing key.
type SignerSession struct {
	// signerKey is the key descriptor for the signer.
	signerKey *keychain.KeyDescriptor

	// txs maps transaction IDs to their signing sessions.
	txs map[TxID]*TxSignerSession
}

// NewSignerSession creates a new signing session for a tree. It
// automatically extracts the path for the given signer and creates sessions
// for each transaction in that path.
func NewSignerSession(signer input.MuSig2Signer,
	signerKey *keychain.KeyDescriptor, sweepTapscriptRoot []byte,
	prevOuts txscript.PrevOutputFetcher,
	tree *Node) (*SignerSession, error) {

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
	txs := make(map[TxID]*TxSignerSession)

	// For each transaction in the signer's path, create a TxSignerSession
	// which will help facilitate MuSig2 signing for that transaction.
	err := signerPath.ForEach(func(node *Node) error {
		tx, err := node.ToTx()
		if err != nil {
			return fmt.Errorf("failed to create tx: %w", err)
		}

		session, err := node.NewTxSignerSession(
			signer, sweepTapscriptRoot, signerKey, prevOuts,
		)
		if err != nil {
			return fmt.Errorf("failed to create tx session: %w",
				err)
		}

		txs[tx.TxHash()] = session

		return nil
	})
	if err != nil {
		cleanupErr := cleanupSignerSessions(txs)
		if cleanupErr != nil {
			return nil, errors.Join(
				err, fmt.Errorf("failed to clean up signer "+
					"session: %w", cleanupErr),
			)
		}

		return nil, err
	}

	return &SignerSession{
		signerKey: signerKey,
		txs:       txs,
	}, nil
}

// Cleanup removes every underlying MuSig2 session. It is safe to call Cleanup
// more than once, including after a partially successful signing attempt.
func (s *SignerSession) Cleanup() error {
	return cleanupSignerSessions(s.txs)
}

// cleanupSignerSessions removes all transaction sessions and combines any
// cleanup errors so one failure does not prevent the remaining cleanup work.
func cleanupSignerSessions(txs map[TxID]*TxSignerSession) error {
	errs := make([]error, 0)
	for txid, txSession := range txs {
		err := txSession.Cleanup()
		if err != nil {
			errs = append(
				errs, fmt.Errorf("failed to clean up tx "+
					"%s: %w", txid, err),
			)
		}
	}

	return errors.Join(errs...)
}

// PubKey returns the signer's public key.
func (s *SignerSession) PubKey() *btcec.PublicKey {
	return s.signerKey.PubKey
}

// GetNonces returns nonces for all transactions in the signer's path.
// This is used after all signers have shared their public nonces and the
// aggregated nonce has been computed for each transaction.
func (s *SignerSession) GetNonces() map[TxID]Musig2PubNonce {
	nonces := make(map[TxID]Musig2PubNonce, len(s.txs))
	for txid, txSession := range s.txs {
		nonces[txid] = txSession.GetNonce()
	}

	return nonces
}

// RegisterAggNonces registers the aggregated nonce for each transaction in the
// signer's path.
func (s *SignerSession) RegisterAggNonces(
	nonceSet map[TxID]Musig2PubNonce) error {

	for txid, txSession := range s.txs {
		nonce, ok := nonceSet[txid]
		if !ok {
			return fmt.Errorf("aggregated nonce for tx %s "+
				"not found", txid)
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
// This is used in the second round of MuSig2 signing where each signer
// generates their partial signatures.
func (s *SignerSession) Signatures(cleanup bool) (
	map[TxID]*musig2.PartialSignature, error) {

	sigs := make(map[TxID]*musig2.PartialSignature, len(s.txs))
	for txid, txSession := range s.txs {
		sig, err := txSession.Sign(cleanup)
		if err != nil {
			return nil, fmt.Errorf("failed to sign tx %s: %w", txid,
				err)
		}

		sigs[txid] = sig
	}

	return sigs, nil
}
