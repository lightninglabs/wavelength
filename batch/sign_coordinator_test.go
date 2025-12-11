package batch

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/scripts"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo/internal/testutils"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// signerCoordinatorTestHarness encapsulates all test fixtures for
// TxSignerCoordinator tests. It sets up a 3-of-3 MuSig2 signing scenario with
// one operator and two clients. The operator runs the coordinator while clients
// participate in the signing protocol by providing nonces and partial
// signatures.
type signerCoordinatorTestHarness struct {
	t *testing.T

	// Operator key and wallet (runs the coordinator).
	operatorKey    *btcec.PublicKey
	operatorWallet input.MuSig2Signer

	// Client keys and wallets (participate in signing).
	client1Key    *btcec.PublicKey
	client1Wallet input.MuSig2Signer
	client2Key    *btcec.PublicKey
	client2Wallet input.MuSig2Signer

	// Tree node with all three cosigners.
	node              *tree.Node
	sweepTapRootBytes []byte
	prevOutFetcher    txscript.PrevOutputFetcher
}

// newSignCoordinatorTestHarness creates a new test harness with all
// fixtures initialized.
func newSignCoordinatorTestHarness(t *testing.T) *signerCoordinatorTestHarness {
	t.Helper()

	// Create cosigner keys.
	operatorKey, operatorWallet := testutils.CreateKey(1)
	client1Key, client1Wallet := testutils.CreateKey(2)
	client2Key, client2Wallet := testutils.CreateKey(3)

	// Compute sweep tapscript root.
	sweepKey, _ := testutils.CreateKey(99)
	sweepTapLeaf, err := scripts.UnilateralCSVTimeoutTapLeaf(sweepKey, 144)
	require.NoError(t, err)
	sweepTapRoot := sweepTapLeaf.TapHash()
	sweepTapRootBytes := sweepTapRoot[:]

	// Compute FinalKey for the node.
	cosigners := []*btcec.PublicKey{operatorKey, client1Key, client2Key}
	finalKey, err := tree.ComputeFinalKey(cosigners, sweepTapRootBytes)
	require.NoError(t, err)

	// Create a simple leaf node with 3 cosigners.
	node := &tree.Node{
		Input: wire.OutPoint{
			Hash:  chainhash.HashH([]byte("parent")),
			Index: 0,
		},
		Outputs: []*wire.TxOut{
			{Value: 5000, PkScript: []byte("vtxo_script")},
			scripts.AnchorOutput(),
		},
		CoSigners: cosigners,
		FinalKey:  finalKey,
		Children:  make(map[uint32]*tree.Node),
	}

	// Create prev output fetcher.
	prevOut := &wire.TxOut{
		Value:    5000,
		PkScript: []byte("parent_script"),
	}
	prevOutFetcher := txscript.NewCannedPrevOutputFetcher(
		prevOut.PkScript, prevOut.Value,
	)

	return &signerCoordinatorTestHarness{
		t:                 t,
		operatorKey:       operatorKey,
		operatorWallet:    operatorWallet,
		client1Key:        client1Key,
		client1Wallet:     client1Wallet,
		client2Key:        client2Key,
		client2Wallet:     client2Wallet,
		node:              node,
		sweepTapRootBytes: sweepTapRootBytes,
		prevOutFetcher:    prevOutFetcher,
	}
}

// operatorKeyDesc returns the operator's key descriptor.
//
//nolint:ll
func (h *signerCoordinatorTestHarness) operatorKeyDesc() *keychain.KeyDescriptor {
	return &keychain.KeyDescriptor{
		PubKey: h.operatorKey,
		KeyLocator: keychain.KeyLocator{
			Family: 1,
			Index:  0,
		},
	}
}

// newCoordinator creates a new TxSignerCoordinator for testing.
func (h *signerCoordinatorTestHarness) newCoordinator() *TxSignerCoordinator {
	h.t.Helper()

	coordinator, err := NewTxSignerCoordinator(
		h.operatorWallet, h.operatorKeyDesc(), h.node,
		h.sweepTapRootBytes, h.prevOutFetcher,
	)
	require.NoError(h.t, err)

	return coordinator
}

// createClientSession creates a MuSig2 session for a client using the node's
// NewSignerSession method.
func (h *signerCoordinatorTestHarness) createClientSession(
	wallet input.MuSig2Signer) *input.MuSig2SessionInfo {

	h.t.Helper()

	session, err := h.node.NewSignerSession(
		&keychain.KeyDescriptor{
			KeyLocator: keychain.KeyLocator{Family: 1, Index: 0},
		},
		wallet, h.sweepTapRootBytes,
	)
	require.NoError(h.t, err)

	return session
}

// addAllClientNonces adds nonces from both clients to the coordinator.
func (h *signerCoordinatorTestHarness) addAllClientNonces(
	coordinator *TxSignerCoordinator) {

	h.t.Helper()

	client1Session := h.createClientSession(h.client1Wallet)
	client2Session := h.createClientSession(h.client2Wallet)

	err := coordinator.AddNonce(h.client1Key, client1Session.PublicNonce)
	require.NoError(h.t, err)

	err = coordinator.AddNonce(h.client2Key, client2Session.PublicNonce)
	require.NoError(h.t, err)
}

// TestTxSignerCoordinatorFullFlow tests the complete MuSig2 signing flow
// using sequential subtests for each phase.
func TestTxSignerCoordinatorFullFlow(t *testing.T) {
	t.Parallel()

	h := newSignCoordinatorTestHarness(t)

	// Create MuSig2 sessions for client participants.
	client1Session := h.createClientSession(h.client1Wallet)
	client2Session := h.createClientSession(h.client2Wallet)

	// Create coordinator (creates operator's session internally).
	coordinator := h.newCoordinator()

	// Shared state across phases.
	var (
		aggNonce               tree.Musig2PubNonce
		sigHash                [32]byte
		client1Sig, client2Sig *musig2.PartialSignature
	)

	t.Run("initial state", func(t *testing.T) {
		require.False(t, coordinator.HasAllNonces())
		require.False(t, coordinator.FullySigned())
	})

	t.Run("nonce phase", func(t *testing.T) {
		// Add client1 nonce.
		err := coordinator.AddNonce(
			h.client1Key, client1Session.PublicNonce,
		)
		require.NoError(t, err)
		require.False(t, coordinator.HasAllNonces())

		// Add client2 nonce.
		err = coordinator.AddNonce(
			h.client2Key, client2Session.PublicNonce,
		)
		require.NoError(t, err)
		require.True(t, coordinator.HasAllNonces())

		// Aggregate nonces.
		var err2 error
		aggNonce, err2 = coordinator.AggregateNonces()
		require.NoError(t, err2)
		require.NotEqual(t, tree.Musig2PubNonce{}, aggNonce)
	})

	t.Run("signature phase", func(t *testing.T) {
		// Register aggregated nonce with client sessions.
		err := h.client1Wallet.MuSig2RegisterCombinedNonce(
			client1Session.SessionID, aggNonce,
		)
		require.NoError(t, err)

		err = h.client2Wallet.MuSig2RegisterCombinedNonce(
			client2Session.SessionID, aggNonce,
		)
		require.NoError(t, err)

		sigHashBytes, err := h.node.SigHash(h.prevOutFetcher)
		require.NoError(t, err)
		sigHash = [32]byte(sigHashBytes)

		// Operator signs via the coordinator.
		err = coordinator.Sign()
		require.NoError(t, err)

		// Generate client signatures.
		client1Sig, err = h.client1Wallet.MuSig2Sign(
			client1Session.SessionID, sigHash, false,
		)
		require.NoError(t, err)

		client2Sig, err = h.client2Wallet.MuSig2Sign(
			client2Session.SessionID, sigHash, false,
		)
		require.NoError(t, err)

		// Add client partial signatures to coordinator.
		err = coordinator.AddPartialSignature(h.client1Key, client1Sig)
		require.NoError(t, err)
		require.False(t, coordinator.FullySigned())

		err = coordinator.AddPartialSignature(h.client2Key, client2Sig)
		require.NoError(t, err)
		require.True(t, coordinator.FullySigned())
	})

	t.Run("combination phase", func(t *testing.T) {
		finalSig, err := coordinator.AggregateSig()
		require.NoError(t, err)
		require.NotNil(t, finalSig)

		// Verify the final signature is valid.
		require.True(t, finalSig.Verify(sigHash[:], h.node.FinalKey))
	})
}

// TestTxSignerCoordinatorErrors tests error handling for all coordinator
// methods.
func TestTxSignerCoordinatorErrors(t *testing.T) {
	t.Parallel()

	t.Run("AddNonce rejects unknown signer", func(t *testing.T) {
		t.Parallel()

		h := newSignCoordinatorTestHarness(t)
		coordinator := h.newCoordinator()

		unknownKey, _ := testutils.CreateKey(999)
		err := coordinator.AddNonce(unknownKey, tree.Musig2PubNonce{})
		require.Error(t, err)
		require.ErrorContains(t, err, "not part of expected cosigners")
	})

	t.Run("AddNonce rejects operator nonce", func(t *testing.T) {
		t.Parallel()

		h := newSignCoordinatorTestHarness(t)
		coordinator := h.newCoordinator()

		var nonce tree.Musig2PubNonce
		err := coordinator.AddNonce(h.operatorKey, nonce)
		require.Error(t, err)
		require.ErrorContains(t, err, "operator nonce is managed")
	})

	t.Run("AggregateNonces fails before all nonces", func(t *testing.T) {
		t.Parallel()

		h := newSignCoordinatorTestHarness(t)
		coordinator := h.newCoordinator()

		_, err := coordinator.AggregateNonces()
		require.Error(t, err)
		require.ErrorContains(t, err, "not all nonces")
	})

	t.Run("AddPartialSig fails before all nonces", func(t *testing.T) {
		t.Parallel()

		h := newSignCoordinatorTestHarness(t)
		coordinator := h.newCoordinator()

		dummySig := &musig2.PartialSignature{}
		err := coordinator.AddPartialSignature(h.client1Key, dummySig)
		require.Error(t, err)
		require.ErrorContains(t, err, "not all nonces")
	})

	t.Run("AddPartialSig rejects unknown signer", func(t *testing.T) {
		t.Parallel()

		h := newSignCoordinatorTestHarness(t)
		coordinator := h.newCoordinator()
		h.addAllClientNonces(coordinator)

		unknownKey, _ := testutils.CreateKey(999)
		dummySig := &musig2.PartialSignature{}
		err := coordinator.AddPartialSignature(unknownKey, dummySig)
		require.Error(t, err)
		require.ErrorContains(t, err, "not part of expected cosigners")
	})

	t.Run("AddPartialSig rejects operator sig", func(t *testing.T) {
		t.Parallel()

		h := newSignCoordinatorTestHarness(t)
		coordinator := h.newCoordinator()
		h.addAllClientNonces(coordinator)

		dummySig := &musig2.PartialSignature{}
		err := coordinator.AddPartialSignature(h.operatorKey, dummySig)
		require.Error(t, err)
		require.ErrorContains(t, err, "operator signature is managed")
	})

	t.Run("AggregateSig fails before fully signed", func(t *testing.T) {
		t.Parallel()

		h := newSignCoordinatorTestHarness(t)
		coordinator := h.newCoordinator()

		_, err := coordinator.AggregateSig()
		require.Error(t, err)
		require.ErrorContains(t, err, "not all partial signatures")
	})
}
