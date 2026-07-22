package round

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"sync"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/lightninglabs/wavelength/lib/tree"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
)

// TreeSigningSessionRequest identifies one blocking request for external
// tree-signing material for a single cosigner key and one transaction session
// on that cosigner's path through the VTXO tree. The same SessionID correlates
// the nonce request (round one) with the later partial-signature request
// (round two), so the external party can sign under the exact secret nonce it
// committed to.
type TreeSigningSessionRequest struct {
	// RoundID is the round this signing material belongs to.
	RoundID RoundID

	// CosignerKey is the public key of the external cosigner whose private
	// key material lives outside this daemon (e.g. an aggregate FROST key).
	CosignerKey *btcec.PublicKey

	// SessionID is the daemon-assigned, per-transaction MuSig2 session
	// identifier. It is stable across the nonce and partial-signature
	// rounds for one transaction session.
	SessionID [32]byte

	// Cosigners is the full ordered MuSig2 participant set for this session
	// (the client cosigner plus the operator), as supplied to
	// MuSig2CreateSession.
	Cosigners []*btcec.PublicKey

	// SweepTapscriptRoot is the taproot tweak applied to the aggregate key
	// (the VTXO tree's sweep tapscript root). It is load-bearing: it
	// changes the aggregate key and therefore the signature.
	SweepTapscriptRoot []byte

	// SigHash is the 32-byte taproot sighash the partial signature must
	// cover. It is set only on partial-signature requests.
	SigHash [32]byte

	// AggNonce is the operator-aggregated combined nonce for this session.
	// It is set only on partial-signature requests (round two).
	AggNonce [musig2.PubNonceSize]byte
}

// ExternalTreeSignerBackend fetches MuSig2 tree-signing material for a cosigner
// key whose private key lives outside this daemon (for example an aggregate
// FROST key the client controls off-box). Each method blocks until the external
// party supplies the requested material or the context is cancelled. All MuSig2
// (or FROST) cryptography happens at the external party; this daemon only
// ferries the resulting nonce and partial-signature bytes.
type ExternalTreeSignerBackend interface {
	// FetchTreeNonce blocks until the external party supplies a fresh
	// public nonce for the given session (round one).
	FetchTreeNonce(context.Context,
		TreeSigningSessionRequest) (tree.Musig2PubNonce, error)

	// FetchTreePartialSig blocks until the external party supplies a
	// partial signature over req.SigHash under req.AggNonce for the given
	// session (round two).
	FetchTreePartialSig(context.Context,
		TreeSigningSessionRequest) (*musig2.PartialSignature, error)
}

// externalMuSig2Signer is a daemon-side input.MuSig2Signer that performs no
// MuSig2 cryptography itself. It stands in for the wallet signer for a single
// external cosigner key and routes nonce generation and partial-signature
// production to an ExternalTreeSignerBackend (in production, an RPC-driven
// broker). The key material never enters this daemon.
//
// It implements only the subset of input.MuSig2Signer that the VTXO tree
// signing path exercises: CreateSession, RegisterCombinedNonce, Sign, and
// Cleanup. The remaining methods return an unsupported error because the client
// never aggregates nonces or combines signatures on this path (the operator
// does).
type externalMuSig2Signer struct {
	// ctx bounds the blocking backend calls. It is stored on the struct
	// because the input.MuSig2Signer methods this type implements
	// (MuSig2CreateSession/MuSig2Sign) take no context parameter, so the
	// round lifecycle context must be captured at construction to remain
	// cancellable.
	//
	//nolint:containedctx
	ctx         context.Context
	backend     ExternalTreeSignerBackend
	roundID     RoundID
	cosignerKey *btcec.PublicKey

	mu       sync.Mutex
	counter  uint64
	sessions map[input.MuSig2SessionID]*externalTreeSession
}

// externalTreeSession is the per-transaction session state the proxy retains
// between the nonce and partial-signature rounds.
type externalTreeSession struct {
	cosigners          []*btcec.PublicKey
	sweepTapscriptRoot []byte
	aggNonce           [musig2.PubNonceSize]byte
	haveAggNonce       bool
}

// selectTreeSigner returns the MuSig2 signer for one VTXO's tree path. It is
// the wallet signer by default; when the VTXO's signing key is marked external
// it is a proxy that routes nonce and partial-signature production to the
// configured external party. It errors if a VTXO is marked external but no
// external tree signer is configured, or the external key is missing.
func selectTreeSigner(ctx context.Context, env *ClientEnvironment,
	roundID RoundID, vtxoReq types.VTXORequest,
	signerKey SignerKey) (input.MuSig2Signer, error) {

	if !vtxoReq.ExternalTreeSigner {
		return env.Wallet, nil
	}

	if env.ExternalTreeSigner == nil {
		return nil, fmt.Errorf("vtxo signer %x is external but no "+
			"external tree signer is configured", signerKey[:])
	}
	if vtxoReq.SigningKey.PubKey == nil {
		return nil, fmt.Errorf("external tree signer %x has no "+
			"public key", signerKey[:])
	}

	return newExternalMuSig2Signer(
		ctx, env.ExternalTreeSigner, roundID, vtxoReq.SigningKey.PubKey,
	), nil
}

// newExternalMuSig2Signer builds a proxy signer bound to one external cosigner
// key and round. The context bounds all blocking backend calls, so a failing or
// abandoned round cancels any in-flight external request.
func newExternalMuSig2Signer(ctx context.Context,
	backend ExternalTreeSignerBackend, roundID RoundID,
	cosignerKey *btcec.PublicKey) *externalMuSig2Signer {

	return &externalMuSig2Signer{
		ctx:         ctx,
		backend:     backend,
		roundID:     roundID,
		cosignerKey: cosignerKey,
		sessions: make(
			map[input.MuSig2SessionID]*externalTreeSession,
		),
	}
}

// nextSessionID deterministically derives a unique per-transaction session id
// from the round, the cosigner key, and a monotonic counter. Determinism keeps
// the id reproducible for tests and avoids pulling in a randomness source.
func (s *externalMuSig2Signer) nextSessionID() input.MuSig2SessionID {
	h := sha256.New()
	roundID := s.roundID
	_, _ = h.Write(roundID[:])
	_, _ = h.Write(s.cosignerKey.SerializeCompressed())

	var ctr [8]byte
	binary.BigEndian.PutUint64(ctr[:], s.counter)
	s.counter++
	_, _ = h.Write(ctr[:])

	var id input.MuSig2SessionID
	copy(id[:], h.Sum(nil))

	return id
}

// MuSig2CreateSession allocates a session, fetches a fresh public nonce for the
// external cosigner from the backend, and returns a session info carrying that
// nonce. The local key locator is ignored: the cosigner is identified by the
// public key this proxy is bound to, because an external aggregate key has no
// wallet-resident private key or key locator.
func (s *externalMuSig2Signer) MuSig2CreateSession(version input.MuSig2Version,
	_ keychain.KeyLocator, signers []*btcec.PublicKey,
	tweaks *input.MuSig2Tweaks, _ [][musig2.PubNonceSize]byte,
	_ *musig2.Nonces) (*input.MuSig2SessionInfo, error) {

	s.mu.Lock()
	sessionID := s.nextSessionID()

	var sweepRoot []byte
	if tweaks != nil {
		sweepRoot = tweaks.TaprootTweak
	}

	s.sessions[sessionID] = &externalTreeSession{
		cosigners:          signers,
		sweepTapscriptRoot: sweepRoot,
	}
	s.mu.Unlock()

	pubNonce, err := s.backend.FetchTreeNonce(
		s.ctx, TreeSigningSessionRequest{
			RoundID:            s.roundID,
			CosignerKey:        s.cosignerKey,
			SessionID:          sessionID,
			Cosigners:          signers,
			SweepTapscriptRoot: sweepRoot,
		},
	)
	if err != nil {
		s.mu.Lock()
		delete(s.sessions, sessionID)
		s.mu.Unlock()

		return nil, fmt.Errorf("fetch external tree nonce: %w", err)
	}

	return &input.MuSig2SessionInfo{
		SessionID:   sessionID,
		Version:     version,
		PublicNonce: pubNonce,
	}, nil
}

// MuSig2RegisterCombinedNonce records the operator-aggregated combined nonce so
// the later partial-signature request can carry it to the external party.
func (s *externalMuSig2Signer) MuSig2RegisterCombinedNonce(
	sessionID input.MuSig2SessionID,
	combinedNonce [musig2.PubNonceSize]byte) error {

	s.mu.Lock()
	defer s.mu.Unlock()

	session, ok := s.sessions[sessionID]
	if !ok {
		return fmt.Errorf("unknown external tree session %x",
			sessionID[:])
	}

	session.aggNonce = combinedNonce
	session.haveAggNonce = true

	return nil
}

// MuSig2Sign fetches the external cosigner's partial signature over sigHash
// under the previously registered aggregate nonce. The session must have a
// combined nonce registered first. When cleanup is set the session state is
// dropped after signing.
func (s *externalMuSig2Signer) MuSig2Sign(sessionID input.MuSig2SessionID,
	sigHash [sha256.Size]byte, cleanup bool) (*musig2.PartialSignature,
	error) {

	s.mu.Lock()
	session, ok := s.sessions[sessionID]
	if !ok {
		s.mu.Unlock()

		return nil, fmt.Errorf("unknown external tree session %x",
			sessionID[:])
	}
	if !session.haveAggNonce {
		s.mu.Unlock()

		return nil, fmt.Errorf("external tree session %x has no "+
			"combined nonce", sessionID[:])
	}
	req := TreeSigningSessionRequest{
		RoundID:            s.roundID,
		CosignerKey:        s.cosignerKey,
		SessionID:          sessionID,
		Cosigners:          session.cosigners,
		SweepTapscriptRoot: session.sweepTapscriptRoot,
		SigHash:            sigHash,
		AggNonce:           session.aggNonce,
	}
	s.mu.Unlock()

	partialSig, err := s.backend.FetchTreePartialSig(s.ctx, req)
	if err != nil {
		return nil, fmt.Errorf("fetch external tree partial sig: %w",
			err)
	}

	if cleanup {
		s.mu.Lock()
		delete(s.sessions, sessionID)
		s.mu.Unlock()
	}

	return partialSig, nil
}

// MuSig2Cleanup drops the session state for the given id.
func (s *externalMuSig2Signer) MuSig2Cleanup(
	sessionID input.MuSig2SessionID) error {

	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.sessions, sessionID)

	return nil
}

// MuSig2RegisterNonces is unsupported: on the client tree-signing path the
// operator aggregates nonces and hands back a combined nonce, which arrives via
// MuSig2RegisterCombinedNonce.
func (s *externalMuSig2Signer) MuSig2RegisterNonces(input.MuSig2SessionID,
	[][musig2.PubNonceSize]byte) (bool, error) {

	return false, fmt.Errorf("external tree signer does not support " +
		"nonce registration; the operator aggregates nonces")
}

// MuSig2GetCombinedNonce returns the combined nonce previously registered for
// the session, if any.
func (s *externalMuSig2Signer) MuSig2GetCombinedNonce(
	sessionID input.MuSig2SessionID) ([musig2.PubNonceSize]byte, error) {

	s.mu.Lock()
	defer s.mu.Unlock()

	session, ok := s.sessions[sessionID]
	if !ok || !session.haveAggNonce {
		return [musig2.PubNonceSize]byte{}, fmt.Errorf("external tree "+
			"session %x has no combined nonce", sessionID[:])
	}

	return session.aggNonce, nil
}

// MuSig2CombineSig is unsupported: the operator, not the client, combines the
// partial signatures on the tree-signing path.
func (s *externalMuSig2Signer) MuSig2CombineSig(input.MuSig2SessionID,
	[]*musig2.PartialSignature) (*schnorr.Signature, bool, error) {

	return nil, false, fmt.Errorf("external tree signer does not support " +
		"signature combination; the operator combines partial sigs")
}

// Compile-time assertion that the proxy satisfies the signer interface used by
// the tree-signing sessions.
var _ input.MuSig2Signer = (*externalMuSig2Signer)(nil)
