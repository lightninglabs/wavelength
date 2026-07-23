package round

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/google/uuid"
	"github.com/lightninglabs/wavelength/lib/tree"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// recordingTreeBackend is a test ExternalTreeSignerBackend that records the
// requests it receives and returns canned material, standing in for the
// external (e.g. FROST) party.
type recordingTreeBackend struct {
	nonce      tree.Musig2PubNonce
	partialSig *musig2.PartialSignature

	nonceReqs   []TreeSigningSessionRequest
	partialReqs []TreeSigningSessionRequest

	nonceErr   error
	partialErr error
}

func (b *recordingTreeBackend) FetchTreeNonce(_ context.Context,
	req TreeSigningSessionRequest) (tree.Musig2PubNonce, error) {

	b.nonceReqs = append(b.nonceReqs, req)
	if b.nonceErr != nil {
		return tree.Musig2PubNonce{}, b.nonceErr
	}

	return b.nonce, nil
}

func (b *recordingTreeBackend) FetchTreePartialSig(_ context.Context,
	req TreeSigningSessionRequest) (*musig2.PartialSignature, error) {

	b.partialReqs = append(b.partialReqs, req)
	if b.partialErr != nil {
		return nil, b.partialErr
	}

	return b.partialSig, nil
}

// newTreeSignerTestKey returns a fresh public key for tree-signer tests.
func newTreeSignerTestKey(t *testing.T) *btcec.PublicKey {
	t.Helper()

	priv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	return priv.PubKey()
}

// TestExternalMuSig2SignerFerriesMaterial verifies the proxy ferries the
// external party's nonce and partial signature through the two MuSig2 rounds,
// carries the round-two sighash and aggregate nonce to the backend, keeps the
// session id stable across rounds, and passes through the cosigner set and
// sweep tweak unchanged.
func TestExternalMuSig2SignerFerriesMaterial(t *testing.T) {
	t.Parallel()

	roundID := RoundID(uuid.New())
	cosignerKey := newTreeSignerTestKey(t)
	operatorKey := newTreeSignerTestKey(t)
	cosigners := []*btcec.PublicKey{cosignerKey, operatorKey}
	sweepRoot := []byte{0x11, 0x22, 0x33}

	var wantNonce tree.Musig2PubNonce
	wantNonce[0] = 0xab
	wantSig := &musig2.PartialSignature{S: new(btcec.ModNScalar)}
	wantSig.S.SetInt(42)

	backend := &recordingTreeBackend{
		nonce:      wantNonce,
		partialSig: wantSig,
	}

	signer := newExternalMuSig2Signer(
		context.Background(), backend, roundID, cosignerKey,
	)

	// Round one: create session fetches a nonce from the backend.
	info, err := signer.MuSig2CreateSession(
		input.MuSig2Version100RC2, keychain.KeyLocator{}, cosigners,
		&input.MuSig2Tweaks{
			TaprootTweak: sweepRoot,
		},
		nil,
		nil,
	)
	require.NoError(t, err)
	require.Equal(t, wantNonce, tree.Musig2PubNonce(info.PublicNonce))

	require.Len(t, backend.nonceReqs, 1)
	nonceReq := backend.nonceReqs[0]
	require.Equal(t, roundID, nonceReq.RoundID)
	require.Equal(t, cosignerKey, nonceReq.CosignerKey)
	require.Equal(t, cosigners, nonceReq.Cosigners)
	require.Equal(t, sweepRoot, nonceReq.SweepTapscriptRoot)
	require.Equal(t, info.SessionID, nonceReq.SessionID)

	// Signing before the combined nonce is registered must fail.
	var sigHash [32]byte
	sigHash[0] = 0x77
	_, err = signer.MuSig2Sign(info.SessionID, sigHash, true)
	require.ErrorContains(t, err, "no combined nonce")
	require.Empty(t, backend.partialReqs)

	// Register the operator-aggregated combined nonce (round 1.5).
	var aggNonce [musig2.PubNonceSize]byte
	aggNonce[0] = 0xcd
	require.NoError(
		t, signer.MuSig2RegisterCombinedNonce(info.SessionID, aggNonce),
	)

	// Round two: signing fetches the partial signature, carrying the
	// sighash and aggregate nonce to the backend.
	gotSig, err := signer.MuSig2Sign(info.SessionID, sigHash, true)
	require.NoError(t, err)
	require.Equal(t, wantSig, gotSig)

	require.Len(t, backend.partialReqs, 1)
	partialReq := backend.partialReqs[0]
	require.Equal(t, info.SessionID, partialReq.SessionID)
	require.Equal(t, sigHash, partialReq.SigHash)
	require.Equal(t, aggNonce, partialReq.AggNonce)
	require.Equal(t, cosigners, partialReq.Cosigners)
	require.Equal(t, sweepRoot, partialReq.SweepTapscriptRoot)

	// After cleanup-on-sign the session is gone.
	_, err = signer.MuSig2Sign(info.SessionID, sigHash, true)
	require.ErrorContains(t, err, "unknown external tree session")
}

// TestExternalMuSig2SignerDistinctSessions verifies each CreateSession call
// gets a distinct, stable session id so concurrent transaction sessions on one
// cosigner path do not collide.
func TestExternalMuSig2SignerDistinctSessions(t *testing.T) {
	t.Parallel()

	cosignerKey := newTreeSignerTestKey(t)
	backend := &recordingTreeBackend{}
	signer := newExternalMuSig2Signer(
		context.Background(), backend,
		RoundID(
			uuid.New(),
		),
		cosignerKey,
	)

	a, err := signer.MuSig2CreateSession(
		input.MuSig2Version100RC2, keychain.KeyLocator{},
		[]*btcec.PublicKey{cosignerKey}, &input.MuSig2Tweaks{}, nil,
		nil,
	)
	require.NoError(t, err)

	b, err := signer.MuSig2CreateSession(
		input.MuSig2Version100RC2, keychain.KeyLocator{},
		[]*btcec.PublicKey{cosignerKey}, &input.MuSig2Tweaks{}, nil,
		nil,
	)
	require.NoError(t, err)

	require.NotEqual(t, a.SessionID, b.SessionID)
}

// TestExternalMuSig2SignerUnsupported verifies the proxy rejects the MuSig2
// operations the client never performs on the tree-signing path.
func TestExternalMuSig2SignerUnsupported(t *testing.T) {
	t.Parallel()

	signer := newExternalMuSig2Signer(
		context.Background(), &recordingTreeBackend{},
		RoundID(
			uuid.New(),
		),
		newTreeSignerTestKey(t),
	)

	_, err := signer.MuSig2RegisterNonces(
		input.MuSig2SessionID{}, nil,
	)
	require.Error(t, err)

	_, _, err = signer.MuSig2CombineSig(input.MuSig2SessionID{}, nil)
	require.Error(t, err)
}
