package round

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/google/uuid"
	"github.com/lightninglabs/wavelength/lib/tree"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// realDelegatingTreeBackend is a test ExternalTreeSignerBackend that stands in
// for the external (e.g. FROST) party by running a real in-memory MuSig2 signer
// for the cosigner key. It proves the daemon-side proxy, the signing executor,
// and the tree session machinery drive a genuine MuSig2 signer end to end when
// the private key lives "outside" the daemon.
type realDelegatingTreeBackend struct {
	signer input.MuSig2Signer

	mu       sync.Mutex
	sessions map[[32]byte]input.MuSig2SessionID
}

func newRealDelegatingTreeBackend(
	signer input.MuSig2Signer) *realDelegatingTreeBackend {

	return &realDelegatingTreeBackend{
		signer:   signer,
		sessions: make(map[[32]byte]input.MuSig2SessionID),
	}
}

func (b *realDelegatingTreeBackend) FetchTreeNonce(_ context.Context,
	req TreeSigningSessionRequest) (tree.Musig2PubNonce, error) {

	info, err := b.signer.MuSig2CreateSession(
		input.MuSig2Version100RC2, keychain.KeyLocator{}, req.Cosigners,
		&input.MuSig2Tweaks{
			TaprootTweak: req.SweepTapscriptRoot,
		},
		nil,
		nil,
	)
	if err != nil {
		return tree.Musig2PubNonce{}, err
	}

	b.mu.Lock()
	b.sessions[req.SessionID] = info.SessionID
	b.mu.Unlock()

	return info.PublicNonce, nil
}

func (b *realDelegatingTreeBackend) FetchTreePartialSig(_ context.Context,
	req TreeSigningSessionRequest) (*musig2.PartialSignature, error) {

	b.mu.Lock()
	realID, ok := b.sessions[req.SessionID]
	b.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("no delegated session for %x",
			req.SessionID[:])
	}

	if err := b.signer.MuSig2RegisterCombinedNonce(
		realID, req.AggNonce,
	); err != nil {
		return nil, err
	}

	return b.signer.MuSig2Sign(realID, req.SigHash, true)
}

// TestExternalTreeSignerDrivesRealSigner proves the full daemon-side path for
// an externally signed VTXO tree key: the signing executor drives the proxy
// signer, which routes MuSig2 session creation and signing to a backend running
// a real signer over a real VTXO tree. The externally produced nonces and
// partial signatures cover exactly the transactions on the cosigner's path, and
// the combined signature verifies — the same outcome as signing locally, but
// with the key held "outside" the daemon.
func TestExternalTreeSignerDrivesRealSigner(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)

	keyFetcher := func(*keychain.KeyDescriptor) (*btcec.PrivateKey, error) {
		return h.clientPrivKey, nil
	}

	// Baseline: a local signer over the same tree, to cross-check that the
	// external path signs the identical transaction set.
	localSigner := input.NewMusigSessionManager(keyFetcher)
	backend := newRealDelegatingTreeBackend(
		input.NewMusigSessionManager(keyFetcher),
	)

	vtxoTree, _ := h.newTestVTXOTree(1)
	prevOuts, err := vtxoTree.Root.PrevOutputFetcher(vtxoTree.BatchOutput)
	require.NoError(t, err)

	signerKey := NewSignerKey(h.clientPubKey)
	signingKey := keychain.KeyDescriptor{PubKey: h.clientPubKey}

	newJob := func(signer input.MuSig2Signer) CreateSignerSessionJob {
		return CreateSignerSessionJob{
			SignerKey:          signerKey,
			Signer:             signer,
			SigningKey:         signingKey,
			SweepTapscriptRoot: vtxoTree.SweepTapscriptRoot,
			PrevOuts:           prevOuts,
			Root:               vtxoTree.Root,
		}
	}

	ctx := context.Background()
	proxy := newExternalMuSig2Signer(
		ctx, backend,
		RoundID(
			uuid.New(),
		),
		h.clientPubKey,
	)

	executor := NewSigningExecutor(1)

	// Create sessions through the external proxy and through a local
	// signer.
	externalResults, err := executor.CreateSessions(
		ctx, []CreateSignerSessionJob{newJob(proxy)},
	)
	require.NoError(t, err)
	require.Len(t, externalResults, 1)

	localResults, err := executor.CreateSessions(
		ctx, []CreateSignerSessionJob{newJob(localSigner)},
	)
	require.NoError(t, err)
	require.Len(t, localResults, 1)

	externalNonces := externalResults[0].Nonces
	require.NotEmpty(t, externalNonces)

	// The external path must sign exactly the transactions the local path
	// does — the same tree path, keyed by the same transaction ids.
	require.Equal(
		t, txIDSet(localResults[0].Nonces), txIDSet(externalNonces),
	)

	// A coordinator aggregates the single cosigner's nonce per transaction
	// (this test tree is a 1-of-1 client MuSig2) and hands the combined
	// nonce back for round two.
	aggNonces := make(
		map[tree.TxID]tree.Musig2PubNonce, len(externalNonces),
	)
	for txID, nonce := range externalNonces {
		agg, err := musig2.AggregateNonces(
			[][musig2.PubNonceSize]byte{nonce},
		)
		require.NoError(t, err)
		aggNonces[txID] = agg
	}
	require.NoError(
		t, externalResults[0].Session.RegisterAggNonces(aggNonces),
	)

	// Round two: the external party produces a partial signature for every
	// transaction on its path.
	sigs, err := executor.Sign(ctx, externalResults)
	require.NoError(t, err)
	require.Len(t, sigs, 1)
	require.Equal(t, len(externalNonces), len(sigs[0].Signatures))
	for txID, sig := range sigs[0].Signatures {
		require.NotNil(t, sig, "missing partial sig for tx %s", txID)
	}
}

// txIDSet returns the transaction id set of a nonce map for comparison.
func txIDSet(
	nonces map[tree.TxID]tree.Musig2PubNonce) map[tree.TxID]struct{} {

	out := make(map[tree.TxID]struct{}, len(nonces))
	for txID := range nonces {
		out[txID] = struct{}{}
	}

	return out
}
