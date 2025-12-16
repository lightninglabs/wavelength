package assets

import (
	"fmt"
	"sync"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
)

// LocalMuSig2Signer manages multiple MuSig2 signing sessions for a single
// private key. It implements the lnd input.MuSig2Signer interface, making it
// compatible with tree signing operations that require concurrent sessions.
//
// Each session is tracked by a unique session ID derived from the combined key
// and public nonce. This allows multiple transactions to be signed in parallel
// while maintaining isolation between sessions.
type LocalMuSig2Signer struct {
	privKey  *btcec.PrivateKey
	sessions map[input.MuSig2SessionID]*localMuSig2Session
	mu       sync.Mutex
}

// localMuSig2Session holds the state for a single MuSig2 signing session.
type localMuSig2Session struct {
	ctx     input.MuSig2Context
	session input.MuSig2Session
	info    input.MuSig2SessionInfo
}

// NewLocalMuSig2Signer creates a new session-based MuSig2 signer for the
// given private key.
func NewLocalMuSig2Signer(privKey *btcec.PrivateKey) *LocalMuSig2Signer {
	return &LocalMuSig2Signer{
		privKey:  privKey,
		sessions: make(map[input.MuSig2SessionID]*localMuSig2Session),
	}
}

// MuSig2CreateSession creates a new MuSig2 signing session with the given
// parameters. The session is tracked internally and can be referenced by the
// returned session ID.
func (s *LocalMuSig2Signer) MuSig2CreateSession(version input.MuSig2Version,
	_ keychain.KeyLocator, allSignerPubKeys []*btcec.PublicKey,
	tweaks *input.MuSig2Tweaks,
	otherSignerNonces [][musig2.PubNonceSize]byte,
	localNonces *musig2.Nonces) (*input.MuSig2SessionInfo, error) {

	s.mu.Lock()
	defer s.mu.Unlock()

	if tweaks == nil {
		tweaks = &input.MuSig2Tweaks{}
	}

	ctx, sess, err := input.MuSig2CreateContext(
		version, s.privKey, allSignerPubKeys, tweaks, localNonces,
	)
	if err != nil {
		return nil, err
	}

	haveAll := false
	for _, nonce := range otherSignerNonces {
		haveAll, err = sess.RegisterPubNonce(nonce)
		if err != nil {
			return nil, err
		}
	}

	combinedKey, err := ctx.CombinedKey()
	if err != nil {
		return nil, err
	}

	publicNonce := sess.PublicNonce()
	sessionID := input.NewMuSig2SessionID(combinedKey, publicNonce)

	var internalKey *btcec.PublicKey
	if tweaks.HasTaprootTweak() {
		internalKey, _ = ctx.TaprootInternalKey()
	}

	info := input.MuSig2SessionInfo{
		SessionID:          sessionID,
		Version:            version,
		PublicNonce:        publicNonce,
		CombinedKey:        combinedKey,
		TaprootTweak:       tweaks.HasTaprootTweak(),
		TaprootInternalKey: internalKey,
		HaveAllNonces:      haveAll,
		HaveAllSigs:        false,
	}

	s.sessions[sessionID] = &localMuSig2Session{
		ctx:     ctx,
		session: sess,
		info:    info,
	}

	return &s.sessions[sessionID].info, nil
}

// MuSig2RegisterNonces registers additional public nonces from other signers
// for the specified session.
func (s *LocalMuSig2Signer) MuSig2RegisterNonces(
	sessionID input.MuSig2SessionID,
	nonces [][musig2.PubNonceSize]byte) (bool, error) {

	s.mu.Lock()
	defer s.mu.Unlock()

	sess, err := s.session(sessionID)
	if err != nil {
		return false, err
	}

	for _, nonce := range nonces {
		haveAll, err := sess.session.RegisterPubNonce(nonce)
		if err != nil {
			return false, err
		}
		sess.info.HaveAllNonces = haveAll
	}

	return sess.info.HaveAllNonces, nil
}

// MuSig2RegisterCombinedNonce registers a pre-aggregated combined nonce for a
// session identified by its ID. This is an alternative to MuSig2RegisterNonces
// and is used when a coordinator has already aggregated all individual nonces
// and wants to distribute the combined nonce to participants.
func (s *LocalMuSig2Signer) MuSig2RegisterCombinedNonce(
	sessionID input.MuSig2SessionID,
	combinedNonce [musig2.PubNonceSize]byte) error {

	s.mu.Lock()
	defer s.mu.Unlock()

	sess, err := s.session(sessionID)
	if err != nil {
		return err
	}

	err = sess.session.RegisterCombinedNonce(combinedNonce)
	if err != nil {
		return err
	}

	sess.info.HaveAllNonces = true

	return nil
}

// MuSig2GetCombinedNonce retrieves the combined nonce for a session identified
// by its ID. This will be available after either all individual nonces have
// been registered via MuSig2RegisterNonces, or a combined nonce has been
// registered via MuSig2RegisterCombinedNonce.
func (s *LocalMuSig2Signer) MuSig2GetCombinedNonce(
	sessionID input.MuSig2SessionID) ([musig2.PubNonceSize]byte, error) {

	s.mu.Lock()
	defer s.mu.Unlock()

	sess, err := s.session(sessionID)
	if err != nil {
		return [musig2.PubNonceSize]byte{}, err
	}

	return sess.session.CombinedNonce()
}

// MuSig2Sign creates a partial signature for the given message hash using
// the specified session. If cleanup is true, the session is removed after
// signing.
func (s *LocalMuSig2Signer) MuSig2Sign(sessionID input.MuSig2SessionID,
	msg [32]byte, cleanup bool) (*musig2.PartialSignature, error) {

	s.mu.Lock()
	defer s.mu.Unlock()

	sess, err := s.session(sessionID)
	if err != nil {
		return nil, err
	}

	sig, err := input.MuSig2Sign(sess.session, msg, true)
	if err != nil {
		return nil, err
	}

	if cleanup {
		delete(s.sessions, sessionID)
	}

	return sig, nil
}

// MuSig2CombineSig combines partial signatures from other signers. Once all
// signatures are combined, the final Schnorr signature is returned.
func (s *LocalMuSig2Signer) MuSig2CombineSig(sessionID input.MuSig2SessionID,
	sigs []*musig2.PartialSignature) (*schnorr.Signature, bool, error) {

	s.mu.Lock()
	defer s.mu.Unlock()

	sess, err := s.session(sessionID)
	if err != nil {
		return nil, false, err
	}

	var haveAll bool
	for _, partial := range sigs {
		haveAll, err = input.MuSig2CombineSig(sess.session, partial)
		if err != nil {
			return nil, false, err
		}
	}

	if haveAll {
		sess.info.HaveAllSigs = true
		return sess.session.FinalSig(), true, nil
	}

	return nil, false, nil
}

// MuSig2Cleanup removes the specified session from the signer.
func (s *LocalMuSig2Signer) MuSig2Cleanup(
	sessionID input.MuSig2SessionID) error {

	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.sessions, sessionID)

	return nil
}

// session returns the session for the given ID, or an error if not found.
func (s *LocalMuSig2Signer) session(id input.MuSig2SessionID) (
	*localMuSig2Session, error) {

	sess, ok := s.sessions[id]
	if !ok {
		return nil, fmt.Errorf("session %x not found", id)
	}

	return sess, nil
}
