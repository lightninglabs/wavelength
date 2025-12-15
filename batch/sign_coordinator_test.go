package batch

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/scripts"
	treepkg "github.com/lightninglabs/darepo-client/lib/tree"
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
	node              *treepkg.Node
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
	finalKey, err := treepkg.ComputeFinalKey(cosigners, sweepTapRootBytes)
	require.NoError(t, err)

	// Create a simple leaf node with 3 cosigners.
	node := &treepkg.Node{
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
		Children:  make(map[uint32]*treepkg.Node),
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
	wallet input.MuSig2Signer,
	pubKey *btcec.PublicKey) *input.MuSig2SessionInfo {

	h.t.Helper()

	session, err := h.node.NewSignerSession(
		&keychain.KeyDescriptor{
			PubKey: pubKey,
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

	client1Session := h.createClientSession(h.client1Wallet, h.client1Key)
	client2Session := h.createClientSession(h.client2Wallet, h.client2Key)

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
	client1Session := h.createClientSession(h.client1Wallet, h.client1Key)
	client2Session := h.createClientSession(h.client2Wallet, h.client2Key)

	// Create coordinator (creates operator's session internally).
	coordinator := h.newCoordinator()

	// Shared state across phases.
	var (
		aggNonce               treepkg.Musig2PubNonce
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
		require.NotEqual(t, treepkg.Musig2PubNonce{}, aggNonce)
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
		var nonce treepkg.Musig2PubNonce
		err := coordinator.AddNonce(unknownKey, nonce)
		require.Error(t, err)
		require.ErrorContains(t, err, "not part of expected cosigners")
	})

	t.Run("AddNonce rejects operator nonce", func(t *testing.T) {
		t.Parallel()

		h := newSignCoordinatorTestHarness(t)
		coordinator := h.newCoordinator()

		var nonce treepkg.Musig2PubNonce
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

// TestTreeSignCoordinatorGetSignaturesForSigners tests the
// GetFinalSigsForSigners method on TreeSignCoordinator.
func TestTreeSignCoordinatorGetSignaturesForSigners(t *testing.T) {
	t.Parallel()

	// This test verifies that GetFinalSigsForSigners correctly filters
	// signatures based on which signing keys are provided.

	// Create keys for operator and two clients.
	operatorKey, operatorWallet := testutils.CreateKey(1)
	client1Key, client1Wallet := testutils.CreateKey(2)
	client2Key, client2Wallet := testutils.CreateKey(3)

	// Compute sweep key.
	sweepKey, _ := testutils.CreateKey(99)

	// Build a simple treepkg using the VTXODescriptor system.
	// Create two VTXOs - one for each client.
	desc1, err := treepkg.NewVTXODescriptor(
		5000, client1Key, operatorKey, 144,
	)
	require.NoError(t, err)

	desc2, err := treepkg.NewVTXODescriptor(
		5000, client2Key, operatorKey, 144,
	)
	require.NoError(t, err)

	// Create batch outpoint and output.
	batchOutpoint := wire.OutPoint{
		Hash:  chainhash.HashH([]byte("test-batch")),
		Index: 0,
	}
	batchOutput := &wire.TxOut{
		Value:    10000,
		PkScript: []byte("batch_output"),
	}

	// Build the treepkg using the library function.
	vtxoTree, err := treepkg.BuildVTXOTree(
		batchOutpoint, batchOutput,
		[]treepkg.VTXODescriptor{*desc1, *desc2}, operatorKey, sweepKey,
		288, 2,
	)
	require.NoError(t, err)
	require.NotNil(t, vtxoTree)

	t.Run("filters signatures correctly by signing keys",
		func(t *testing.T) {
			// Create coordinator.
			coordinator, err := NewTreeSignCoordinator(
				operatorWallet,
				&keychain.KeyDescriptor{PubKey: operatorKey},
				vtxoTree,
			)
			require.NoError(t, err)
			require.NotNil(t, coordinator)

			// Should have 3 transactions (root + 2 leaves).
			require.Len(t, coordinator.txSigners, 3)

			// Create tree signer sessions for client participants.
			// (Operator session is embedded in coordinator.)
			c1Session, err := vtxoTree.NewTreeSignerSession(
				client1Wallet,
				&keychain.KeyDescriptor{PubKey: client1Key},
			)
			require.NoError(t, err)

			c2Session, err := vtxoTree.NewTreeSignerSession(
				client2Wallet,
				&keychain.KeyDescriptor{PubKey: client2Key},
			)
			require.NoError(t, err)

			// Add nonces from client parties.
			// (Operator nonces are automatic)
			c1Nonces := c1Session.GetNonces()
			_, err = coordinator.AddNonces(client1Key, c1Nonces)
			require.NoError(t, err)

			c2Nonces := c2Session.GetNonces()
			_, err = coordinator.AddNonces(client2Key, c2Nonces)
			require.NoError(t, err)

			require.True(t, coordinator.HasAllNonces())

			// Register aggregated nonces with client sessions.
			aggNonces, err := coordinator.GetAggregatedNonces()
			require.NoError(t, err)

			err = c1Session.RegisterAggNonces(aggNonces)
			require.NoError(t, err)

			err = c2Session.RegisterAggNonces(aggNonces)
			require.NoError(t, err)

			// Get partial signatures from all parties.
			err = coordinator.Sign()
			require.NoError(t, err)

			// Operator signatures are managed internally, so we
			// don't add them here.

			c1Sigs, err := c1Session.Signatures(false)
			require.NoError(t, err)
			err = coordinator.AddPartialSignatures(
				client1Key, c1Sigs,
			)
			require.NoError(t, err)

			c2Sigs, err := c2Session.Signatures(false)
			require.NoError(t, err)
			err = coordinator.AddPartialSignatures(
				client2Key, c2Sigs,
			)
			require.NoError(t, err)

			require.True(t, coordinator.FullySigned())

			// Client1 should get signatures for its transactions.
			client1Sigs, err := coordinator.GetFinalSigsForSigners(
				[]*btcec.PublicKey{client1Key},
			)
			require.NoError(t, err)
			require.NotEmpty(t, client1Sigs,
				"client1 should get signatures")

			// Client2 should get signatures for its transactions.
			client2Sigs, err := coordinator.GetFinalSigsForSigners(
				[]*btcec.PublicKey{client2Key},
			)
			require.NoError(t, err)
			require.NotEmpty(t, client2Sigs,
				"client2 should get signatures")

			// Operator should get all signatures.
			opSigs, err := coordinator.GetFinalSigsForSigners(
				[]*btcec.PublicKey{operatorKey},
			)
			require.NoError(t, err)
			require.Len(t, opSigs, 3,
				"operator should get all 3 signatures")

			// Verify signatures are valid by calling
			// AggregateSigs() which internally verifies them.
			allSigs, err := coordinator.AggregateSigs()
			require.NoError(t, err)
			require.Len(t, allSigs, 3)

			// Client1's signatures should be a subset of all
			// signatures.
			for txid := range client1Sigs {
				require.Contains(t, allSigs, txid)
			}

			// Client2's signatures should be a subset of all
			// signatures.
			for txid := range client2Sigs {
				require.Contains(t, allSigs, txid)
			}
		})

	t.Run("empty signing keys returns empty result", func(t *testing.T) {
		coordinator, err := NewTreeSignCoordinator(
			operatorWallet,
			&keychain.KeyDescriptor{PubKey: operatorKey},
			vtxoTree,
		)
		require.NoError(t, err)

		sigs, err := coordinator.GetFinalSigsForSigners(
			[]*btcec.PublicKey{},
		)
		require.NoError(t, err)
		require.Empty(t, sigs)
	})

	t.Run("unknown signing key returns empty result", func(t *testing.T) {
		coordinator, err := NewTreeSignCoordinator(
			operatorWallet,
			&keychain.KeyDescriptor{PubKey: operatorKey},
			vtxoTree,
		)
		require.NoError(t, err)

		unknownKey, _ := testutils.CreateKey(999)
		sigs, err := coordinator.GetFinalSigsForSigners(
			[]*btcec.PublicKey{unknownKey},
		)
		require.NoError(t, err)
		require.Empty(t, sigs)
	})

	t.Run("fails when not fully signed", func(t *testing.T) {
		coordinator, err := NewTreeSignCoordinator(
			operatorWallet,
			&keychain.KeyDescriptor{PubKey: operatorKey},
			vtxoTree,
		)
		require.NoError(t, err)

		// Try to get signatures before adding any.
		_, err = coordinator.GetFinalSigsForSigners(
			[]*btcec.PublicKey{client1Key},
		)
		require.Error(t, err)
		require.Contains(t, err.Error(), "failed to sign")
	})
}

// TestTreeSignCoordinatorErrors tests error handling for TreeSignCoordinator
// methods.
func TestTreeSignCoordinatorErrors(t *testing.T) {
	t.Parallel()

	// Setup a simple tree for error testing.
	operatorKey, operatorWallet := testutils.CreateKey(1)
	clientKey, _ := testutils.CreateKey(2)
	sweepKey, _ := testutils.CreateKey(99)

	vtxo, err := treepkg.NewVTXODescriptor(
		5000, clientKey, operatorKey, 144,
	)
	require.NoError(t, err)

	batchOutput, err := treepkg.BuildBatchOutput(
		[]treepkg.VTXODescriptor{*vtxo}, operatorKey, sweepKey, 144,
	)
	require.NoError(t, err)

	batchOutpoint := wire.OutPoint{
		Hash:  chainhash.HashH([]byte("error-test")),
		Index: 0,
	}

	vtxoTree, err := treepkg.BuildVTXOTree(
		batchOutpoint, batchOutput, []treepkg.VTXODescriptor{*vtxo},
		operatorKey, sweepKey, 144, 2,
	)
	require.NoError(t, err)

	t.Run("NewTreeSignCoordinator rejects nil signer", func(t *testing.T) {
		t.Parallel()

		_, err := NewTreeSignCoordinator(
			nil,
			&keychain.KeyDescriptor{PubKey: operatorKey},
			vtxoTree,
		)
		require.Error(t, err)
		require.Contains(t, err.Error(), "signer cannot be nil")
	})

	t.Run("NewTreeSignCoordinator rejects nil key", func(t *testing.T) {
		t.Parallel()

		_, err := NewTreeSignCoordinator(
			operatorWallet,
			nil,
			vtxoTree,
		)
		require.Error(t, err)
		require.Contains(t, err.Error(), "key cannot be nil")
	})

	t.Run("NewTreeSignCoordinator rejects nil tree", func(t *testing.T) {
		t.Parallel()

		_, err := NewTreeSignCoordinator(
			operatorWallet,
			&keychain.KeyDescriptor{PubKey: operatorKey},
			nil,
		)
		require.Error(t, err)
		require.Contains(t, err.Error(), "tree cannot be nil")
	})

	t.Run("GetAggregatedNonces fails before nonces", func(t *testing.T) {
		t.Parallel()

		coordinator, err := NewTreeSignCoordinator(
			operatorWallet,
			&keychain.KeyDescriptor{PubKey: operatorKey},
			vtxoTree,
		)
		require.NoError(t, err)

		_, err = coordinator.GetAggregatedNonces()
		require.Error(t, err)
		require.Contains(t, err.Error(), "not all nonces")
	})

	t.Run("GetAggNoncesForSigners fails before nonces", func(t *testing.T) {
		t.Parallel()

		coordinator, err := NewTreeSignCoordinator(
			operatorWallet,
			&keychain.KeyDescriptor{PubKey: operatorKey},
			vtxoTree,
		)
		require.NoError(t, err)

		_, err = coordinator.GetAggNoncesForSigners(
			[]*btcec.PublicKey{clientKey},
		)
		require.Error(t, err)
		require.Contains(t, err.Error(), "not all nonces")
	})

	t.Run("AddPartialSignatures rejects unknown tx", func(t *testing.T) {
		t.Parallel()

		coordinator, err := NewTreeSignCoordinator(
			operatorWallet,
			&keychain.KeyDescriptor{PubKey: operatorKey},
			vtxoTree,
		)
		require.NoError(t, err)

		unknownTxID := chainhash.HashH([]byte("unknown"))
		sigs := map[TxID]*musig2.PartialSignature{
			unknownTxID: {},
		}

		err = coordinator.AddPartialSignatures(clientKey, sigs)
		require.Error(t, err)
		require.Contains(t, err.Error(), "not found")
	})

	t.Run("AggregateSigs fails before fully signed", func(t *testing.T) {
		t.Parallel()

		coordinator, err := NewTreeSignCoordinator(
			operatorWallet,
			&keychain.KeyDescriptor{PubKey: operatorKey},
			vtxoTree,
		)
		require.NoError(t, err)

		_, err = coordinator.AggregateSigs()
		require.Error(t, err)
	})

	t.Run("Sign fails before all nonces", func(t *testing.T) {
		t.Parallel()

		coordinator, err := NewTreeSignCoordinator(
			operatorWallet,
			&keychain.KeyDescriptor{PubKey: operatorKey},
			vtxoTree,
		)
		require.NoError(t, err)

		// Try to sign before all nonces are collected.
		err = coordinator.Sign()
		require.Error(t, err)
	})
}

// TestTreeSignCoordinatorGetNoncesForSigners tests the GetAggNoncesForSigners
// method which returns aggregated nonces filtered by signing keys.
func TestTreeSignCoordinatorGetNoncesForSigners(t *testing.T) {
	t.Parallel()

	// Setup with 2 clients having different VTXOs.
	operatorKey, operatorWallet := testutils.CreateKey(1)
	client1Key, client1Wallet := testutils.CreateKey(2)
	client2Key, client2Wallet := testutils.CreateKey(3)
	sweepKey, _ := testutils.CreateKey(99)

	desc1, err := treepkg.NewVTXODescriptor(
		5000, client1Key, operatorKey, 144,
	)
	require.NoError(t, err)

	desc2, err := treepkg.NewVTXODescriptor(
		5000, client2Key, operatorKey, 144,
	)
	require.NoError(t, err)

	batchOutpoint := wire.OutPoint{
		Hash:  chainhash.HashH([]byte("nonces-test")),
		Index: 0,
	}

	vtxos := []treepkg.VTXODescriptor{*desc1, *desc2}
	batchOutput, err := treepkg.BuildBatchOutput(
		vtxos, operatorKey, sweepKey, 144,
	)
	require.NoError(t, err)

	vtxoTree, err := treepkg.BuildVTXOTree(
		batchOutpoint, batchOutput, vtxos,
		operatorKey, sweepKey, 144, 2,
	)
	require.NoError(t, err)

	// Create coordinator and collect all nonces.
	coordinator, err := NewTreeSignCoordinator(
		operatorWallet,
		&keychain.KeyDescriptor{PubKey: operatorKey},
		vtxoTree,
	)
	require.NoError(t, err)

	c1Session, err := vtxoTree.NewTreeSignerSession(
		client1Wallet, &keychain.KeyDescriptor{PubKey: client1Key},
	)
	require.NoError(t, err)

	c2Session, err := vtxoTree.NewTreeSignerSession(
		client2Wallet, &keychain.KeyDescriptor{PubKey: client2Key},
	)
	require.NoError(t, err)

	_, err = coordinator.AddNonces(client1Key, c1Session.GetNonces())
	require.NoError(t, err)

	_, err = coordinator.AddNonces(client2Key, c2Session.GetNonces())
	require.NoError(t, err)

	require.True(t, coordinator.HasAllNonces())

	t.Run("returns nonces for specific client", func(t *testing.T) {
		// Client1 should get nonces for their transactions.
		c1Nonces, err := coordinator.GetAggNoncesForSigners(
			[]*btcec.PublicKey{client1Key},
		)
		require.NoError(t, err)
		require.NotEmpty(t, c1Nonces)

		// Client2 should get nonces for their transactions.
		c2Nonces, err := coordinator.GetAggNoncesForSigners(
			[]*btcec.PublicKey{client2Key},
		)
		require.NoError(t, err)
		require.NotEmpty(t, c2Nonces)
	})

	t.Run("operator gets all nonces", func(t *testing.T) {
		opNonces, err := coordinator.GetAggNoncesForSigners(
			[]*btcec.PublicKey{operatorKey},
		)
		require.NoError(t, err)
		require.Len(t, opNonces, 3, "operator involved in all 3 txs")
	})

	t.Run("unknown key returns empty", func(t *testing.T) {
		unknownKey, _ := testutils.CreateKey(999)
		nonces, err := coordinator.GetAggNoncesForSigners(
			[]*btcec.PublicKey{unknownKey},
		)
		require.NoError(t, err)
		require.Empty(t, nonces)
	})

	t.Run("empty keys returns empty", func(t *testing.T) {
		nonces, err := coordinator.GetAggNoncesForSigners(
			[]*btcec.PublicKey{},
		)
		require.NoError(t, err)
		require.Empty(t, nonces)
	})

	t.Run("multiple keys combines results", func(t *testing.T) {
		// Both clients together should get nonces for all their txs.
		bothNonces, err := coordinator.GetAggNoncesForSigners(
			[]*btcec.PublicKey{client1Key, client2Key},
		)
		require.NoError(t, err)

		// Should have nonces for all transactions (root + 2 leaves).
		require.Len(t, bothNonces, 3)
	})
}

// TestEndToEndTreeSigning tests the complete signing flow with multiple
// clients, including one client with 2 VTXOs. Uses sequential subtests to
// verify each phase of the MuSig2 signing protocol.
func TestEndToEndTreeSigning(t *testing.T) {
	t.Parallel()

	// Operator.
	operatorKey, operatorWallet := testutils.CreateKey(1)
	operatorKeyDesc := &keychain.KeyDescriptor{
		PubKey:     operatorKey,
		KeyLocator: keychain.KeyLocator{Family: 1, Index: 0},
	}

	sweepKey, _ := testutils.CreateKey(2)

	// Client A: 2 VTXOs (with different cosigner keys).
	clientAKey1, walletA1 := testutils.CreateKey(10)
	clientAKeyDesc1 := &keychain.KeyDescriptor{
		PubKey:     clientAKey1,
		KeyLocator: keychain.KeyLocator{Family: 10, Index: 0},
	}

	clientAKey2, walletA2 := testutils.CreateKey(11)
	clientAKeyDesc2 := &keychain.KeyDescriptor{
		PubKey:     clientAKey2,
		KeyLocator: keychain.KeyLocator{Family: 11, Index: 0},
	}

	// Client B: 1 VTXO.
	clientBKey, walletB := testutils.CreateKey(20)
	clientBKeyDesc := &keychain.KeyDescriptor{
		PubKey:     clientBKey,
		KeyLocator: keychain.KeyLocator{Family: 20, Index: 0},
	}

	// Create VTXO descriptors.
	vtxoA1, err := treepkg.NewVTXODescriptor(
		btcutil.Amount(5000), clientAKey1, operatorKey, 144,
	)
	require.NoError(t, err)

	vtxoA2, err := treepkg.NewVTXODescriptor(
		btcutil.Amount(3000), clientAKey2, operatorKey, 144,
	)
	require.NoError(t, err)

	vtxoB, err := treepkg.NewVTXODescriptor(
		btcutil.Amount(2000), clientBKey, operatorKey, 144,
	)
	require.NoError(t, err)

	vtxos := []treepkg.VTXODescriptor{*vtxoA1, *vtxoA2, *vtxoB}

	// Build proper batch output using BuildBatchOutput.
	batchOutput, err := treepkg.BuildBatchOutput(
		vtxos, operatorKey, sweepKey, 144,
	)
	require.NoError(t, err)

	// Build tree.
	batchOutpoint := wire.OutPoint{
		Hash:  chainhash.HashH([]byte("commitment_tx")),
		Index: 0,
	}

	tree, err := treepkg.BuildVTXOTree(
		batchOutpoint, batchOutput, vtxos,
		operatorKey, sweepKey, 144, 2,
	)
	require.NoError(t, err)
	require.NotNil(t, tree)

	// Verify tree structure.
	require.NoError(t, tree.Verify())

	// Shared state across phases.
	var (
		coordinator                    *TreeSignCoordinator
		sessionA1, sessionA2, sessionB *treepkg.SignerSession
		aggNonces                      map[TxID]treepkg.Musig2PubNonce
	)

	t.Run("session creation", func(t *testing.T) {
		// Operator creates coordinator for full tree. The coordinator
		// automatically creates the operator's signing session and
		// generates nonces.
		var err error
		coordinator, err = NewTreeSignCoordinator(
			operatorWallet, operatorKeyDesc, tree,
		)
		require.NoError(t, err)
		require.NotNil(t, coordinator)

		// Client A: Extract paths and create sessions for each VTXO.
		pathA1, err := tree.ExtractPathForCoSigners(clientAKey1)
		require.NoError(t, err)
		require.NotNil(t, pathA1)

		sessionA1, err = pathA1.NewTreeSignerSession(
			walletA1, clientAKeyDesc1,
		)
		require.NoError(t, err)

		pathA2, err := tree.ExtractPathForCoSigners(clientAKey2)
		require.NoError(t, err)
		require.NotNil(t, pathA2)

		sessionA2, err = pathA2.NewTreeSignerSession(
			walletA2, clientAKeyDesc2,
		)
		require.NoError(t, err)

		// Client B: Extract path and create session.
		pathB, err := tree.ExtractPathForCoSigners(clientBKey)
		require.NoError(t, err)
		require.NotNil(t, pathB)

		sessionB, err = pathB.NewTreeSignerSession(
			walletB, clientBKeyDesc,
		)
		require.NoError(t, err)
	})

	t.Run("nonce collection", func(t *testing.T) {
		// Operator nonces are generated automatically by the
		// coordinator. Verify they exist.
		opNonces := coordinator.OperatorNonces()
		require.NotEmpty(t, opNonces)

		// Still waiting for clients (operator nonces don't count).
		require.False(t, coordinator.HasAllNonces())

		// Client A generates nonces for both VTXOs.
		noncesA1 := sessionA1.GetNonces()
		require.NotEmpty(t, noncesA1)

		_, err := coordinator.AddNonces(clientAKey1, noncesA1)
		require.NoError(t, err)

		noncesA2 := sessionA2.GetNonces()
		require.NotEmpty(t, noncesA2)

		_, err = coordinator.AddNonces(clientAKey2, noncesA2)
		require.NoError(t, err)

		// Client B generates nonces.
		noncesB := sessionB.GetNonces()
		require.NotEmpty(t, noncesB)

		_, err = coordinator.AddNonces(clientBKey, noncesB)
		require.NoError(t, err)

		// All nonces collected.
		require.True(t, coordinator.HasAllNonces())
	})

	t.Run("nonce aggregation", func(t *testing.T) {
		var err error
		aggNonces, err = coordinator.GetAggregatedNonces()
		require.NoError(t, err)
		require.NotEmpty(t, aggNonces)

		// Distribute aggregated nonces to client participants.
		// (Operator registers nonces internally when Sign is called)
		err = sessionA1.RegisterAggNonces(aggNonces)
		require.NoError(t, err)

		err = sessionA2.RegisterAggNonces(aggNonces)
		require.NoError(t, err)

		err = sessionB.RegisterAggNonces(aggNonces)
		require.NoError(t, err)
	})

	t.Run("partial signature generation", func(t *testing.T) {
		// Operator signs using the coordinator's embedded session.
		// The signatures are automatically managed by the operator
		// sessions within each TxSignerCoordinator, so we don't need
		// to add them via AddPartialSignatures.
		err := coordinator.Sign()
		require.NoError(t, err)
		require.False(t, coordinator.FullySigned())

		// Client A signs (both VTXOs).
		sigsA1, err := sessionA1.Signatures(true)
		require.NoError(t, err)
		require.NotEmpty(t, sigsA1)

		err = coordinator.AddPartialSignatures(clientAKey1, sigsA1)
		require.NoError(t, err)

		sigsA2, err := sessionA2.Signatures(true)
		require.NoError(t, err)
		require.NotEmpty(t, sigsA2)

		err = coordinator.AddPartialSignatures(clientAKey2, sigsA2)
		require.NoError(t, err)

		// Client B signs.
		sigsB, err := sessionB.Signatures(true)
		require.NoError(t, err)
		require.NotEmpty(t, sigsB)

		err = coordinator.AddPartialSignatures(clientBKey, sigsB)
		require.NoError(t, err)

		// All signatures collected.
		require.True(t, coordinator.FullySigned())
	})

	t.Run("signature combination", func(t *testing.T) {
		finalSigs, err := coordinator.AggregateSigs()
		require.NoError(t, err)
		require.NotEmpty(t, finalSigs)

		// Verify we have signatures for all transactions.
		require.Equal(t, tree.Root.NumTx(), len(finalSigs),
			"should have signature for each transaction")

		// Store signatures in tree.
		err = tree.SubmitTreeSigs(finalSigs)
		require.NoError(t, err)
	})

	t.Run("tree verification", func(t *testing.T) {
		// Clients verify their paths (re-extract to get signed
		// versions).
		signedPathA1, err := tree.ExtractPathForCoSigners(clientAKey1)
		require.NoError(t, err)
		err = signedPathA1.VerifySigned()
		require.NoError(t, err)

		signedPathA2, err := tree.ExtractPathForCoSigners(clientAKey2)
		require.NoError(t, err)
		err = signedPathA2.VerifySigned()
		require.NoError(t, err)

		signedPathB, err := tree.ExtractPathForCoSigners(clientBKey)
		require.NoError(t, err)
		err = signedPathB.VerifySigned()
		require.NoError(t, err)

		// Full tree verification.
		err = tree.VerifySigned()
		require.NoError(t, err)

		// Verify each leaf can create a signed transaction.
		leaves := tree.Root.GetLeafNodes()
		require.Len(t, leaves, 3)

		for i, leaf := range leaves {
			signedTx, err := leaf.ToSignedTx()
			require.NoError(t, err, "leaf %d", i)
			require.NotNil(t, signedTx, "leaf %d", i)

			// Verify signature in witness.
			require.Len(t, signedTx.TxIn, 1, "leaf %d", i)

			witness := signedTx.TxIn[0].Witness
			require.Len(t, witness, 1, "leaf %d", i)
			require.NotEmpty(t, witness[0], "leaf %d", i)
		}
	})

	t.Run("script VM validation", func(t *testing.T) {
		// Create a prev output fetcher for the entire tree.
		treeFetcher, err := tree.Root.PrevOutputFetcher(batchOutput)
		require.NoError(t, err)

		// Validate each transaction in the tree using the Bitcoin
		// script VM. This ensures the signatures are cryptographically
		// valid and the transactions can actually spend their inputs.
		txCount := 0
		err = tree.Root.ForEach(func(node *treepkg.Node) error {
			// Get the signed transaction.
			signedTx, txErr := node.ToSignedTx()
			require.NoError(t, txErr)

			// Get the input being spent.
			prevOutput := treeFetcher.FetchPrevOutput(node.Input)
			require.NotNil(t, prevOutput,
				"prev output not found for %s", node.Input)

			// Create signature hash cache.
			sigHashes := txscript.NewTxSigHashes(
				signedTx, treeFetcher,
			)

			// Create script execution engine.
			engine, engineErr := txscript.NewEngine(
				prevOutput.PkScript, // Script being spent
				signedTx,            // Transaction spending it
				0,                   // Input index
				txscript.StandardVerifyFlags,
				nil, // Sig cache
				sigHashes,
				prevOutput.Value,
				treeFetcher,
			)
			require.NoError(t, engineErr)

			// Execute the script - this validates the signature.
			execErr := engine.Execute()
			txid, _ := node.TXID()
			require.NoError(t, execErr,
				"script execution failed for tx %s", txid)

			txCount++

			return nil
		})
		require.NoError(t, err)
		require.Equal(t, tree.Root.NumTx(), txCount,
			"should have validated all transactions")
	})
}

// TestTreeSigningWithSingleClient tests signing with just one client.
func TestTreeSigningWithSingleClient(t *testing.T) {
	t.Parallel()

	// Setup.
	operatorKey, operatorWallet := testutils.CreateKey(1)
	operatorKeyDesc := &keychain.KeyDescriptor{
		PubKey:     operatorKey,
		KeyLocator: keychain.KeyLocator{Family: 1, Index: 0},
	}

	sweepKey, _ := testutils.CreateKey(2)

	clientKey, clientWallet := testutils.CreateKey(10)
	clientKeyDesc := &keychain.KeyDescriptor{
		PubKey:     clientKey,
		KeyLocator: keychain.KeyLocator{Family: 10, Index: 0},
	}

	// Create single VTXO.
	vtxo, err := treepkg.NewVTXODescriptor(
		btcutil.Amount(5000), clientKey, operatorKey, 144,
	)
	require.NoError(t, err)

	// Build proper batch output.
	batchOutput, err := treepkg.BuildBatchOutput(
		[]treepkg.VTXODescriptor{*vtxo}, operatorKey, sweepKey, 144,
	)
	require.NoError(t, err)

	batchOutpoint := wire.OutPoint{
		Hash:  chainhash.HashH([]byte("commitment")),
		Index: 0,
	}

	tree, err := treepkg.BuildVTXOTree(
		batchOutpoint, batchOutput, []treepkg.VTXODescriptor{*vtxo},
		operatorKey, sweepKey, 144, 2,
	)
	require.NoError(t, err)

	// Root should be a leaf (single VTXO).
	require.True(t, tree.Root.IsLeaf())

	// Create coordinator (which creates operator session automatically).
	coordinator, err := NewTreeSignCoordinator(
		operatorWallet, operatorKeyDesc, tree,
	)
	require.NoError(t, err)

	// Create client session.
	clientSession, err := tree.NewTreeSignerSession(
		clientWallet, clientKeyDesc,
	)
	require.NoError(t, err)

	// Nonces.
	clientNonces := clientSession.GetNonces()

	_, err = coordinator.AddNonces(clientKey, clientNonces)
	require.NoError(t, err)
	require.True(t, coordinator.HasAllNonces())

	// Aggregate and distribute.
	aggNonces, err := coordinator.GetAggregatedNonces()
	require.NoError(t, err)

	err = clientSession.RegisterAggNonces(aggNonces)
	require.NoError(t, err)

	// AggregateSig.
	err = coordinator.Sign()
	require.NoError(t, err)

	// Operator signatures are managed internally, don't add them.

	clientSigs, err := clientSession.Signatures(true)
	require.NoError(t, err)

	err = coordinator.AddPartialSignatures(clientKey, clientSigs)
	require.NoError(t, err)

	require.True(t, coordinator.FullySigned())

	// Combine.
	finalSigs, err := coordinator.AggregateSigs()
	require.NoError(t, err)

	// Store and verify.
	err = tree.SubmitTreeSigs(finalSigs)
	require.NoError(t, err)

	err = tree.VerifySigned()
	require.NoError(t, err)

	// Verify can create signed transaction.
	signedTx, err := tree.Root.ToSignedTx()
	require.NoError(t, err)
	require.NotNil(t, signedTx)

	// Validate the signed transaction can spend the batch output.
	prevFetcher := txscript.NewCannedPrevOutputFetcher(
		batchOutput.PkScript, batchOutput.Value,
	)
	sigHashes := txscript.NewTxSigHashes(signedTx, prevFetcher)

	engine, err := txscript.NewEngine(
		batchOutput.PkScript, signedTx, 0, txscript.StandardVerifyFlags,
		nil, sigHashes, batchOutput.Value, prevFetcher,
	)
	require.NoError(t, err)
	require.NoError(t, engine.Execute(),
		"single VTXO transaction should pass script validation")
}

// TestTreeSigningScriptValidation uses the Bitcoin script VM to validate that
// each signed transaction in the tree can actually spend its parent output,
// and that a client can perform a unilateral exit via the timeout path.
func TestTreeSigningScriptValidation(t *testing.T) {
	t.Parallel()

	operatorKey, operatorWallet := testutils.CreateKey(1)
	operatorKeyDesc := &keychain.KeyDescriptor{
		PubKey:     operatorKey,
		KeyLocator: keychain.KeyLocator{Family: 1, Index: 0},
	}

	sweepKey, _ := testutils.CreateKey(2)

	clientKey, clientWallet := testutils.CreateKey(10)
	clientKeyDesc := &keychain.KeyDescriptor{
		PubKey:     clientKey,
		KeyLocator: keychain.KeyLocator{Family: 10, Index: 0},
	}

	// Create VTXO.
	exitDelay := uint32(144)
	vtxo, err := treepkg.NewVTXODescriptor(
		btcutil.Amount(5000), clientKey, operatorKey, exitDelay,
	)
	require.NoError(t, err)

	// Build proper batch output using BuildBatchOutput.
	batchOutput, err := treepkg.BuildBatchOutput(
		[]treepkg.VTXODescriptor{*vtxo}, operatorKey, sweepKey, 144,
	)
	require.NoError(t, err)

	batchOutpoint := wire.OutPoint{
		Hash:  chainhash.HashH([]byte("commitment")),
		Index: 0,
	}

	tree, err := treepkg.BuildVTXOTree(
		batchOutpoint, batchOutput, []treepkg.VTXODescriptor{*vtxo},
		operatorKey, sweepKey, 144, 2,
	)
	require.NoError(t, err)

	// AggregateSig the tree (full flow).
	coordinator, err := NewTreeSignCoordinator(
		operatorWallet, operatorKeyDesc, tree,
	)
	require.NoError(t, err)

	clientSession, err := tree.NewTreeSignerSession(
		clientWallet, clientKeyDesc,
	)
	require.NoError(t, err)

	// Nonce exchange.
	clientNonces := clientSession.GetNonces()

	_, err = coordinator.AddNonces(clientKey, clientNonces)
	require.NoError(t, err)

	aggNonces, err := coordinator.GetAggregatedNonces()
	require.NoError(t, err)

	err = clientSession.RegisterAggNonces(aggNonces)
	require.NoError(t, err)

	// Signature exchange.
	err = coordinator.Sign()
	require.NoError(t, err)

	// Operator signatures are managed internally, don't add them.

	clientSigs, err := clientSession.Signatures(true)
	require.NoError(t, err)

	err = coordinator.AddPartialSignatures(clientKey, clientSigs)
	require.NoError(t, err)

	finalSigs, err := coordinator.AggregateSigs()
	require.NoError(t, err)

	err = tree.SubmitTreeSigs(finalSigs)
	require.NoError(t, err)

	// Since this is a single VTXO, the root IS the leaf.
	require.True(t, tree.Root.IsLeaf())

	// Get the signed transaction.
	signedVTX, err := tree.Root.ToSignedTx()
	require.NoError(t, err)

	// Validate the signed transaction can spend the batch output.
	prevOutFetcher := txscript.NewCannedPrevOutputFetcher(
		batchOutput.PkScript, batchOutput.Value,
	)

	hashCache := txscript.NewTxSigHashes(signedVTX, prevOutFetcher)

	engine, err := txscript.NewEngine(
		batchOutput.PkScript, signedVTX, 0,
		txscript.StandardVerifyFlags, nil, hashCache, batchOutput.Value,
		prevOutFetcher,
	)
	require.NoError(t, err)

	// Execute the script - should succeed.
	err = engine.Execute()
	require.NoError(t, err, "VTXT transaction should be valid")

	// Now, we simulate a unilateral exit by the client via the timeout
	// path.

	// The VTXO output is the first output of the leaf transaction.
	vtxoOutput := tree.Root.Outputs[0]
	vtxoOutpoint := wire.OutPoint{
		Hash:  signedVTX.TxHash(),
		Index: 0,
	}

	// Client creates a sweep transaction spending via timeout path.
	sweepTx := wire.NewMsgTx(2)
	sweepTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: vtxoOutpoint,
		// Sequence must be set appropriately for CSV.
		Sequence: exitDelay,
	})

	// Add output (client sweeps to their address).
	sweepTx.AddTxOut(&wire.TxOut{
		Value:    int64(vtxo.Amount) - 500, // Minus fee
		PkScript: []byte("client_sweep_address"),
	})

	// Get the VTXO tapscript and spend info for timeout path.
	vtxoTapscript, err := scripts.VTXOTapScript(
		clientKey, operatorKey, exitDelay,
	)
	require.NoError(t, err)

	timeoutSpendInfo, err := scripts.NewVTXOSpendInfo(
		vtxoTapscript, scripts.VTXOTimeoutPathLeaf,
	)
	require.NoError(t, err)

	// AggregateSig the sweep transaction via timeout path.
	vtxoPrevOutFetcher := txscript.NewCannedPrevOutputFetcher(
		vtxoOutput.PkScript, vtxoOutput.Value,
	)

	signDesc := &input.SignDescriptor{
		KeyDesc:    *clientKeyDesc,
		Output:     vtxoOutput,
		InputIndex: 0,
		SigHashes: txscript.NewTxSigHashes(
			sweepTx, vtxoPrevOutFetcher,
		),
		PrevOutputFetcher: vtxoPrevOutFetcher,
		HashType:          txscript.SigHashDefault,
		WitnessScript:     timeoutSpendInfo.WitnessScript,
		ControlBlock:      timeoutSpendInfo.ControlBlock,
		SignMethod:        input.TaprootScriptSpendSignMethod,
	}

	timeoutWitness, err := scripts.VTXOTimeoutSpendWitness(
		clientWallet, signDesc, sweepTx,
	)
	require.NoError(t, err)

	sweepTx.TxIn[0].Witness = timeoutWitness

	// Validate the sweep transaction with script VM.
	sweepHashCache := txscript.NewTxSigHashes(sweepTx, vtxoPrevOutFetcher)

	sweepEngine, err := txscript.NewEngine(
		vtxoOutput.PkScript, sweepTx, 0, txscript.StandardVerifyFlags,
		nil, sweepHashCache, vtxoOutput.Value, vtxoPrevOutFetcher,
	)
	require.NoError(t, err)

	// Execute - should succeed (client can unroll!).
	err = sweepEngine.Execute()
	require.NoError(t, err)
}
