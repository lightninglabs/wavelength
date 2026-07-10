package tree

import (
	"errors"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// mockMuSig2Signer is a mock implementation of input.MuSig2Signer for testing.
type mockMuSig2Signer struct {
	privKey       *btcec.PrivateKey
	sessions      map[input.MuSig2SessionID]*mockSession
	nextSessionID int
}

type mockSession struct {
	info           *input.MuSig2SessionInfo
	nonces         *musig2.Nonces
	musigSession   *musig2.Session
	otherNonces    [][66]byte
	allNoncesKnown bool
	combinedNonce  *[66]byte
}

func newMockMuSig2Signer(privKey *btcec.PrivateKey) *mockMuSig2Signer {
	return &mockMuSig2Signer{
		privKey:  privKey,
		sessions: make(map[input.MuSig2SessionID]*mockSession),
	}
}

func (m *mockMuSig2Signer) MuSig2CreateSession(version input.MuSig2Version,
	keyLoc keychain.KeyLocator, signers []*btcec.PublicKey,
	tweaks *input.MuSig2Tweaks, otherNonces [][musig2.PubNonceSize]byte,
	localNonces *musig2.Nonces) (*input.MuSig2SessionInfo, error) {

	// Generate or use provided nonces.
	var nonces *musig2.Nonces
	var err error
	if localNonces != nil {
		nonces = localNonces
	} else {
		// Generate fresh nonces.
		nonces, err = musig2.GenNonces(
			musig2.WithPublicKey(
				m.privKey.PubKey(),
			),
			musig2.WithNonceSecretKeyAux(m.privKey),
		)
		if err != nil {
			return nil, err
		}
	}

	// Create MuSig2 context with known signers and tweaks.
	var ctxOpts []musig2.ContextOption
	ctxOpts = append(ctxOpts, musig2.WithKnownSigners(signers))

	if tweaks != nil && len(tweaks.TaprootTweak) > 0 {
		ctxOpts = append(
			ctxOpts,
			musig2.WithTaprootTweakCtx(tweaks.TaprootTweak),
		)
	}

	ctx, err := musig2.NewContext(m.privKey, true, ctxOpts...)
	if err != nil {
		return nil, err
	}

	musigSession, err := ctx.NewSession()
	if err != nil {
		return nil, err
	}

	// Generate session ID.
	sessionID := input.MuSig2SessionID{byte(m.nextSessionID)}
	m.nextSessionID++

	// Store session.
	mockSess := &mockSession{
		info: &input.MuSig2SessionInfo{
			SessionID:   sessionID,
			PublicNonce: nonces.PubNonce,
		},
		nonces:       nonces,
		musigSession: musigSession,
		otherNonces:  nil,
	}

	m.sessions[sessionID] = mockSess

	return mockSess.info, nil
}

func (m *mockMuSig2Signer) MuSig2RegisterNonces(sessionID input.MuSig2SessionID,
	nonces [][musig2.PubNonceSize]byte) (bool, error) {

	session, ok := m.sessions[sessionID]
	if !ok {
		return false, fmt.Errorf("session not found")
	}

	// Register all nonces with the musig session.
	var haveAll bool
	var err error
	for _, nonce := range nonces {
		haveAll, err = session.musigSession.RegisterPubNonce(nonce)
		if err != nil {
			return false, err
		}
	}

	session.allNoncesKnown = haveAll

	return haveAll, nil
}

func (m *mockMuSig2Signer) MuSig2RegisterCombinedNonce(
	sessionID input.MuSig2SessionID,
	aggNonce [musig2.PubNonceSize]byte) error {

	session, ok := m.sessions[sessionID]
	if !ok {
		return fmt.Errorf("session not found")
	}

	// Store the combined nonce.
	session.combinedNonce = &aggNonce

	// Register the aggregated nonce with the musig session.
	haveAll, err := session.musigSession.RegisterPubNonce(aggNonce)
	if err != nil {
		return err
	}

	session.allNoncesKnown = haveAll

	return nil
}

func (m *mockMuSig2Signer) MuSig2GetCombinedNonce(
	sessionID input.MuSig2SessionID) ([66]byte, error) {

	session, ok := m.sessions[sessionID]
	if !ok {
		return [66]byte{}, fmt.Errorf("session not found")
	}

	if session.combinedNonce == nil {
		return [66]byte{}, fmt.Errorf("combined nonce not set")
	}

	return *session.combinedNonce, nil
}

func (m *mockMuSig2Signer) MuSig2Sign(sessionID input.MuSig2SessionID,
	msg [32]byte, cleanup bool) (*musig2.PartialSignature, error) {

	session, ok := m.sessions[sessionID]
	if !ok {
		return nil, fmt.Errorf("session not found")
	}

	if !session.allNoncesKnown {
		return nil, fmt.Errorf("not all nonces registered")
	}

	// Create partial signature using our secret nonce.
	partialSig, err := session.musigSession.Sign(msg)
	if err != nil {
		return nil, err
	}

	return partialSig, nil
}

func (m *mockMuSig2Signer) MuSig2CombineSig(sessionID input.MuSig2SessionID,
	partialSigs []*musig2.PartialSignature) (*schnorr.Signature, bool,
	error) {

	session, ok := m.sessions[sessionID]
	if !ok {
		return nil, false, fmt.Errorf("session not found")
	}

	// Combine all partial signatures.
	var haveAll bool
	for _, partialSig := range partialSigs {
		var err error
		haveAll, err = session.musigSession.CombineSig(partialSig)
		if err != nil {
			return nil, false, err
		}
	}

	if !haveAll {
		return nil, false,
			fmt.Errorf("not all partial signatures provided")
	}

	// Get the final signature.
	finalSig := session.musigSession.FinalSig()
	if finalSig == nil {
		return nil, false, fmt.Errorf("final signature is invalid")
	}

	return finalSig, true, nil
}

func (m *mockMuSig2Signer) MuSig2Cleanup(
	sessionID input.MuSig2SessionID) error {

	delete(m.sessions, sessionID)

	return nil
}

// cleanupTrackingSigner tracks session creation and cleanup and can inject
// failures into either operation.
type cleanupTrackingSigner struct {
	*mockMuSig2Signer

	createCalls  int
	failCreateAt int
	cleanupCalls int
	cleanupErr   error
}

// MuSig2CreateSession creates a session unless the configured call should
// fail.
func (s *cleanupTrackingSigner) MuSig2CreateSession(version input.MuSig2Version,
	keyLoc keychain.KeyLocator, signers []*btcec.PublicKey,
	tweaks *input.MuSig2Tweaks, otherNonces [][musig2.PubNonceSize]byte,
	localNonces *musig2.Nonces) (*input.MuSig2SessionInfo, error) {

	s.createCalls++
	if s.createCalls == s.failCreateAt {
		return nil, fmt.Errorf("injected session creation failure")
	}

	return s.mockMuSig2Signer.MuSig2CreateSession(
		version, keyLoc, signers, tweaks, otherNonces, localNonces,
	)
}

// MuSig2Cleanup records cleanup attempts and delegates successful attempts to
// the underlying signer.
func (s *cleanupTrackingSigner) MuSig2Cleanup(
	sessionID input.MuSig2SessionID) error {

	s.cleanupCalls++
	if s.cleanupErr != nil {
		return s.cleanupErr
	}

	return s.mockMuSig2Signer.MuSig2Cleanup(sessionID)
}

// TestTxSignerSession tests the TxSignerSession functionality.
func TestTxSignerSession(t *testing.T) {
	t.Parallel()

	t.Run("creates session and gets nonce", func(t *testing.T) {
		t.Parallel()
		privKey, pubKey := createTestKey(t)
		_, otherKey := createTestKey(t)

		signer := newMockMuSig2Signer(privKey)
		keyDesc := &keychain.KeyDescriptor{
			PubKey: pubKey,
		}

		cosigners := []*btcec.PublicKey{pubKey, otherKey}
		sweepRoot := make([]byte, 32)

		// Create a node with the cosigners.
		node := createSimpleLeaf("test", 1000, cosigners)

		// Create fetcher.
		prevOut := &wire.TxOut{Value: 2000}
		fetcher := txscript.NewCannedPrevOutputFetcher(
			prevOut.PkScript, prevOut.Value,
		)

		session, err := node.NewTxSignerSession(
			signer, sweepRoot, keyDesc, fetcher,
		)
		require.NoError(t, err)
		require.NotNil(t, session)

		// Should be able to get nonce.
		nonce := session.GetNonce()
		require.NotEqual(t, Musig2PubNonce{}, nonce)
	})

	t.Run("registers aggregated nonce", func(t *testing.T) {
		t.Parallel()
		privKey, pubKey := createTestKey(t)
		_, otherKey := createTestKey(t)

		signer := newMockMuSig2Signer(privKey)
		keyDesc := &keychain.KeyDescriptor{
			PubKey: pubKey,
		}

		cosigners := []*btcec.PublicKey{pubKey, otherKey}
		sweepRoot := make([]byte, 32)

		// Create a node with the cosigners.
		node := createSimpleLeaf("test", 1000, cosigners)

		prevOut := &wire.TxOut{Value: 2000}
		fetcher := txscript.NewCannedPrevOutputFetcher(
			prevOut.PkScript, prevOut.Value,
		)

		session, err := node.NewTxSignerSession(
			signer, sweepRoot, keyDesc, fetcher,
		)
		require.NoError(t, err)

		// Get own nonce.
		ownNonce := session.GetNonce()

		// Create other participant's nonce (must be valid EC point).
		otherPrivKey, _ := createTestKey(t)
		otherNonces, err := musig2.GenNonces(
			musig2.WithPublicKey(
				otherPrivKey.PubKey(),
			),
		)
		require.NoError(t, err)

		// Aggregate nonces using AggregateNonces.
		aggNonce, err := musig2.AggregateNonces([][66]byte{
			ownNonce, otherNonces.PubNonce,
		})
		require.NoError(t, err)

		// Register aggregated nonce.
		err = session.RegisterAggNonce(aggNonce)
		require.NoError(t, err)
	})

	t.Run("signs after registering nonces", func(t *testing.T) {
		t.Parallel()
		privKey1, pubKey1 := createTestKey(t)
		privKey2, pubKey2 := createTestKey(t)

		signer1 := newMockMuSig2Signer(privKey1)
		signer2 := newMockMuSig2Signer(privKey2)

		keyDesc1 := &keychain.KeyDescriptor{PubKey: pubKey1}
		keyDesc2 := &keychain.KeyDescriptor{PubKey: pubKey2}

		cosigners := []*btcec.PublicKey{pubKey1, pubKey2}
		sweepRoot := make([]byte, 32)

		// Create a node with the cosigners.
		node := createSimpleLeaf("test", 1000, cosigners)

		prevOut := &wire.TxOut{Value: 2000}
		fetcher := txscript.NewCannedPrevOutputFetcher(
			prevOut.PkScript, prevOut.Value,
		)

		// Create sessions for both signers.
		session1, err := node.NewTxSignerSession(
			signer1, sweepRoot, keyDesc1, fetcher,
		)
		require.NoError(t, err)

		session2, err := node.NewTxSignerSession(
			signer2, sweepRoot, keyDesc2, fetcher,
		)
		require.NoError(t, err)

		// Exchange nonces.
		nonce1 := session1.GetNonce()
		nonce2 := session2.GetNonce()

		// Aggregate nonces.
		aggNonce, err := musig2.AggregateNonces([][66]byte{
			nonce1, nonce2,
		})
		require.NoError(t, err)

		// Register aggregated nonce.
		err = session1.RegisterAggNonce(aggNonce)
		require.NoError(t, err)

		err = session2.RegisterAggNonce(aggNonce)
		require.NoError(t, err)

		// Both should be able to sign.
		sig1, err := session1.Sign(true)
		require.NoError(t, err)
		require.NotNil(t, sig1)

		sig2, err := session2.Sign(true)
		require.NoError(t, err)
		require.NotNil(t, sig2)

		// Note: Signature combination would happen in a coordinator.
		// For this test, we just verify that partial signatures were
		// generated successfully.
	})

	t.Run("calculates correct signature hash", func(t *testing.T) {
		t.Parallel()
		privKey, pubKey := createTestKey(t)

		signer := newMockMuSig2Signer(privKey)
		keyDesc := &keychain.KeyDescriptor{PubKey: pubKey}

		cosigners := []*btcec.PublicKey{pubKey}
		sweepRoot := make([]byte, 32)

		// Create a node with the cosigners.
		node := createSimpleLeaf("test", 1000, cosigners)

		prevOut := &wire.TxOut{
			Value:    2000,
			PkScript: []byte("prevScript"),
		}
		fetcher := txscript.NewCannedPrevOutputFetcher(
			prevOut.PkScript, prevOut.Value,
		)

		session, err := node.NewTxSignerSession(
			signer, sweepRoot, keyDesc, fetcher,
		)
		require.NoError(t, err)

		// Get the tx that the node generates internally.
		tx, err := node.ToTx()
		require.NoError(t, err)

		// Calculate expected signature hash manually.
		expectedSigHash, err := txscript.CalcTaprootSignatureHash(
			txscript.NewTxSigHashes(tx, fetcher),
			txscript.SigHashDefault, tx, 0, fetcher,
		)
		require.NoError(t, err)

		// Should match the session's sigHash.
		require.Equal(t, [32]byte(expectedSigHash), session.sigHash)
	})
}

// TestSignerSession tests the SignerSession functionality.
func TestSignerSession(t *testing.T) {
	t.Parallel()

	t.Run("creates session for signer path", func(t *testing.T) {
		t.Parallel()
		privKey, pubKey := createTestKey(t)
		_, otherKey := createTestKey(t)

		// Create a simple tree with 2 leaves.
		leaf1 := createSimpleLeaf("leaf1", 1000, []*btcec.PublicKey{
			pubKey,
		})
		leaf2 := createSimpleLeaf("leaf2", 2000, []*btcec.PublicKey{
			otherKey,
		})

		root := &Node{
			Input: wire.OutPoint{
				Hash:  chainhash.HashH([]byte("root")),
				Index: 0,
			},
			Outputs: []*wire.TxOut{
				{
					Value: 1000,
				},
				{
					Value: 2000,
				},
			},
			CoSigners: []*btcec.PublicKey{
				pubKey,
				otherKey,
			},
			Children: map[uint32]*Node{
				0: leaf1,
				1: leaf2,
			},
		}

		// Fix child inputs.
		rootTXID, _ := root.TXID()
		leaf1.Input = wire.OutPoint{Hash: rootTXID, Index: 0}
		leaf2.Input = wire.OutPoint{Hash: rootTXID, Index: 1}

		// Create fetcher.
		initialOut := &wire.TxOut{Value: 5000}
		fetcher, err := root.PrevOutputFetcher(initialOut)
		require.NoError(t, err)

		// Create signer session.
		signer := newMockMuSig2Signer(privKey)
		keyDesc := &keychain.KeyDescriptor{PubKey: pubKey}
		sweepRoot := make([]byte, 32)

		session, err := NewSignerSession(
			signer, keyDesc, sweepRoot, fetcher, root,
		)
		require.NoError(t, err)
		require.NotNil(t, session)

		// Should have extracted path with 2 nodes (root + leaf1).
		require.Len(t, session.txs, 2)

		// Verify PubKey.
		require.Equal(t, pubKey, session.PubKey())
	})

	t.Run("returns error for non-existent signer", func(t *testing.T) {
		t.Parallel()
		privKey, pubKey := createTestKey(t)
		_, otherKey1 := createTestKey(t)
		_, otherKey2 := createTestKey(t)

		// Create tree where signer's key is not present.
		leaf := createSimpleLeaf("leaf", 1000, []*btcec.PublicKey{
			otherKey1, otherKey2,
		})

		initialOut := &wire.TxOut{Value: 5000}
		fetcher, err := leaf.PrevOutputFetcher(initialOut)
		require.NoError(t, err)

		signer := newMockMuSig2Signer(privKey)
		keyDesc := &keychain.KeyDescriptor{PubKey: pubKey}
		sweepRoot := make([]byte, 32)

		session, err := NewSignerSession(
			signer, keyDesc, sweepRoot, fetcher, leaf,
		)
		require.Error(t, err)
		require.Nil(t, session)
		require.Contains(t, err.Error(), "no path found for signer")
	})

	t.Run("gets nonces for all transactions", func(t *testing.T) {
		t.Parallel()
		privKey, pubKey := createTestKey(t)

		// Create simple leaf.
		leaf := createSimpleLeaf("leaf", 1000, []*btcec.PublicKey{
			pubKey,
		})

		initialOut := &wire.TxOut{Value: 5000}
		fetcher, err := leaf.PrevOutputFetcher(initialOut)
		require.NoError(t, err)

		signer := newMockMuSig2Signer(privKey)
		keyDesc := &keychain.KeyDescriptor{PubKey: pubKey}
		sweepRoot := make([]byte, 32)

		session, err := NewSignerSession(
			signer, keyDesc, sweepRoot, fetcher, leaf,
		)
		require.NoError(t, err)

		// Get nonces.
		nonces := session.GetNonces()
		require.Len(t, nonces, 1)

		// Verify nonce exists for the leaf tx.
		leafTXID, _ := leaf.TXID()
		nonce, exists := nonces[leafTXID]
		require.True(t, exists)
		require.NotEqual(t, Musig2PubNonce{}, nonce)
	})

	t.Run("registers aggregated nonces for all transactions",
		func(t *testing.T) {
			t.Parallel()
			privKey, pubKey := createTestKey(t)
			_, otherKey := createTestKey(t)

			leaf := createSimpleLeaf(
				"leaf", 1000, []*btcec.PublicKey{
					pubKey, otherKey,
				},
			)

			initialOut := &wire.TxOut{Value: 5000}
			fetcher, err := leaf.PrevOutputFetcher(initialOut)
			require.NoError(t, err)

			signer := newMockMuSig2Signer(privKey)
			keyDesc := &keychain.KeyDescriptor{PubKey: pubKey}
			sweepRoot := make([]byte, 32)

			session, err := NewSignerSession(
				signer, keyDesc, sweepRoot, fetcher, leaf,
			)
			require.NoError(t, err)

			// Get nonces from this session.
			nonces := session.GetNonces()

			// Create other participant's nonce (must be valid EC
			// point).
			otherPrivKey, _ := createTestKey(t)
			otherNonces, err := musig2.GenNonces(
				musig2.WithPublicKey(
					otherPrivKey.PubKey(),
				),
			)
			require.NoError(t, err)

			// Aggregate nonces and register for all txs.
			leafTXID, _ := leaf.TXID()
			aggNonce, err := musig2.AggregateNonces([][66]byte{
				nonces[leafTXID],
				otherNonces.PubNonce,
			})
			require.NoError(t, err)

			aggNonceSet := map[TxID]Musig2PubNonce{
				leafTXID: aggNonce,
			}

			err = session.RegisterAggNonces(aggNonceSet)
			require.NoError(t, err)
		})

	t.Run("generates signatures for all transactions", func(t *testing.T) {
		t.Parallel()
		privKey1, pubKey1 := createTestKey(t)
		privKey2, pubKey2 := createTestKey(t)

		// Create simple leaf with both cosigners.
		leaf := createSimpleLeaf("leaf", 1000, []*btcec.PublicKey{
			pubKey1, pubKey2,
		})

		initialOut := &wire.TxOut{Value: 5000}
		fetcher, err := leaf.PrevOutputFetcher(initialOut)
		require.NoError(t, err)

		signer1 := newMockMuSig2Signer(privKey1)
		signer2 := newMockMuSig2Signer(privKey2)

		keyDesc1 := &keychain.KeyDescriptor{PubKey: pubKey1}
		keyDesc2 := &keychain.KeyDescriptor{PubKey: pubKey2}

		sweepRoot := make([]byte, 32)

		// Create sessions for both signers.
		session1, err := NewSignerSession(
			signer1, keyDesc1, sweepRoot, fetcher, leaf,
		)
		require.NoError(t, err)

		session2, err := NewSignerSession(
			signer2, keyDesc2, sweepRoot, fetcher, leaf,
		)
		require.NoError(t, err)

		// Exchange nonces.
		nonces1 := session1.GetNonces()
		nonces2 := session2.GetNonces()

		// Aggregate and register nonces.
		leafTXID, _ := leaf.TXID()

		aggNonce, err := musig2.AggregateNonces([][66]byte{
			nonces1[leafTXID], nonces2[leafTXID],
		})
		require.NoError(t, err)

		err = session1.RegisterAggNonces(map[TxID]Musig2PubNonce{
			leafTXID: aggNonce,
		})
		require.NoError(t, err)

		err = session2.RegisterAggNonces(map[TxID]Musig2PubNonce{
			leafTXID: aggNonce,
		})
		require.NoError(t, err)

		// Generate signatures.
		sigs1, err := session1.Signatures(true)
		require.NoError(t, err)
		require.Len(t, sigs1, 1)
		require.NotNil(t, sigs1[leafTXID])

		sigs2, err := session2.Signatures(true)
		require.NoError(t, err)
		require.Len(t, sigs2, 1)
		require.NotNil(t, sigs2[leafTXID])
	})

	t.Run("RegisterAggNonces fails if tx not found", func(t *testing.T) {
		t.Parallel()
		privKey, pubKey := createTestKey(t)

		leaf := createSimpleLeaf("leaf", 1000, []*btcec.PublicKey{
			pubKey,
		})

		initialOut := &wire.TxOut{Value: 5000}
		fetcher, err := leaf.PrevOutputFetcher(initialOut)
		require.NoError(t, err)

		signer := newMockMuSig2Signer(privKey)
		keyDesc := &keychain.KeyDescriptor{PubKey: pubKey}
		sweepRoot := make([]byte, 32)

		session, err := NewSignerSession(
			signer, keyDesc, sweepRoot, fetcher, leaf,
		)
		require.NoError(t, err)

		// Register aggregated nonce for non-existent tx.
		var nonce Musig2PubNonce
		nonExistentTxID := chainhash.HashH([]byte("non_existent"))
		err = session.RegisterAggNonces(map[TxID]Musig2PubNonce{
			nonExistentTxID: nonce,
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "aggregated nonce for tx")
		require.Contains(t, err.Error(), "not found")
	})
}

// TestSignerSessionCleanup verifies that cleanup is idempotent and that a
// partially constructed signer session rolls back every created transaction
// session.
func TestSignerSessionCleanup(t *testing.T) {
	t.Parallel()

	t.Run("tx cleanup is idempotent", func(t *testing.T) {
		t.Parallel()

		privKey, pubKey := createTestKey(t)
		signer := &cleanupTrackingSigner{
			mockMuSig2Signer: newMockMuSig2Signer(privKey),
		}
		tree, fetcher := createTwoNodeSignerTree(t, pubKey)
		keyDesc := &keychain.KeyDescriptor{PubKey: pubKey}

		session, err := tree.NewTxSignerSession(
			signer, make([]byte, 32), keyDesc, fetcher,
		)
		require.NoError(t, err)

		require.NoError(t, session.Cleanup())
		require.NoError(t, session.Cleanup())
		require.Equal(t, 1, signer.cleanupCalls)
		require.Empty(t, signer.sessions)
	})

	t.Run("cleanup error can be retried", func(t *testing.T) {
		t.Parallel()

		privKey, pubKey := createTestKey(t)
		cleanupErr := errors.New("cleanup failed")
		signer := &cleanupTrackingSigner{
			mockMuSig2Signer: newMockMuSig2Signer(privKey),
			cleanupErr:       cleanupErr,
		}
		tree, fetcher := createTwoNodeSignerTree(t, pubKey)
		keyDesc := &keychain.KeyDescriptor{PubKey: pubKey}

		session, err := tree.NewTxSignerSession(
			signer, make([]byte, 32), keyDesc, fetcher,
		)
		require.NoError(t, err)

		require.ErrorIs(t, session.Cleanup(), cleanupErr)
		signer.cleanupErr = nil
		require.NoError(t, session.Cleanup())
		require.NoError(t, session.Cleanup())
		require.Equal(t, 2, signer.cleanupCalls)
		require.Empty(t, signer.sessions)
	})

	t.Run("signer cleanup covers every tx", func(t *testing.T) {
		t.Parallel()

		privKey, pubKey := createTestKey(t)
		signer := &cleanupTrackingSigner{
			mockMuSig2Signer: newMockMuSig2Signer(privKey),
		}
		tree, fetcher := createTwoNodeSignerTree(t, pubKey)
		keyDesc := &keychain.KeyDescriptor{PubKey: pubKey}

		session, err := NewSignerSession(
			signer, keyDesc, make([]byte, 32), fetcher, tree,
		)
		require.NoError(t, err)
		require.Len(t, session.txs, 2)

		require.NoError(t, session.Cleanup())
		require.NoError(t, session.Cleanup())
		require.Equal(t, 2, signer.cleanupCalls)
		require.Empty(t, signer.sessions)
	})

	t.Run("construction failure rolls back sessions", func(t *testing.T) {
		t.Parallel()

		privKey, pubKey := createTestKey(t)
		signer := &cleanupTrackingSigner{
			mockMuSig2Signer: newMockMuSig2Signer(privKey),
			failCreateAt:     2,
		}
		tree, fetcher := createTwoNodeSignerTree(t, pubKey)
		keyDesc := &keychain.KeyDescriptor{PubKey: pubKey}

		session, err := NewSignerSession(
			signer, keyDesc, make([]byte, 32), fetcher, tree,
		)
		require.ErrorContains(
			t, err, "injected session creation failure",
		)
		require.Nil(t, session)
		require.Equal(t, 2, signer.createCalls)
		require.Equal(t, 1, signer.cleanupCalls)
		require.Empty(t, signer.sessions)
	})
}

// createTwoNodeSignerTree creates a root and leaf that both belong to the same
// signer, plus the previous-output fetcher needed to construct sessions.
func createTwoNodeSignerTree(t *testing.T,
	signer *btcec.PublicKey) (*Node, txscript.PrevOutputFetcher) {

	t.Helper()

	leaf := createSimpleLeaf("leaf", 1000, []*btcec.PublicKey{signer})
	root := &Node{
		Input: wire.OutPoint{
			Hash: chainhash.HashH([]byte("cleanup-root")),
		},
		Outputs: []*wire.TxOut{
			{
				Value: 1000,
			},
		},
		CoSigners: []*btcec.PublicKey{
			signer,
		},
		Children: map[uint32]*Node{
			0: leaf,
		},
	}

	rootTXID, err := root.TXID()
	require.NoError(t, err)
	leaf.Input = wire.OutPoint{Hash: rootTXID}

	fetcher, err := root.PrevOutputFetcher(&wire.TxOut{Value: 5000})
	require.NoError(t, err)

	return root, fetcher
}

// TestSignerSessionMultiTx tests SignerSession with multiple transactions.
func TestSignerSessionMultiTx(t *testing.T) {
	t.Parallel()

	t.Run("handles multiple transactions in path", func(t *testing.T) {
		t.Parallel()
		privKey, pubKey := createTestKey(t)
		_, otherKey := createTestKey(t)

		// Create tree where signer is in both nodes.
		leaf1 := createSimpleLeaf("leaf1", 1000, []*btcec.PublicKey{
			pubKey,
		})
		leaf2 := createSimpleLeaf("leaf2", 2000, []*btcec.PublicKey{
			otherKey,
		})

		root := &Node{
			Input: wire.OutPoint{
				Hash:  chainhash.HashH([]byte("root")),
				Index: 0,
			},
			Outputs: []*wire.TxOut{
				{
					Value: 1000,
				},
				{
					Value: 2000,
				},
			},
			CoSigners: []*btcec.PublicKey{
				pubKey,
				otherKey,
			},
			Children: map[uint32]*Node{
				0: leaf1,
				1: leaf2,
			},
		}

		rootTXID, _ := root.TXID()
		leaf1.Input = wire.OutPoint{Hash: rootTXID, Index: 0}
		leaf2.Input = wire.OutPoint{Hash: rootTXID, Index: 1}

		initialOut := &wire.TxOut{Value: 5000}
		fetcher, err := root.PrevOutputFetcher(initialOut)
		require.NoError(t, err)

		signer := newMockMuSig2Signer(privKey)
		keyDesc := &keychain.KeyDescriptor{PubKey: pubKey}
		sweepRoot := make([]byte, 32)

		session, err := NewSignerSession(
			signer, keyDesc, sweepRoot, fetcher, root,
		)
		require.NoError(t, err)

		// Should have sessions for root and leaf1 (signer's path).
		require.Len(t, session.txs, 2)

		// Get nonces for all txs.
		nonces := session.GetNonces()
		require.Len(t, nonces, 2)

		// Should have nonce for root and leaf1.
		rootTx, _ := root.ToTx()
		leaf1Tx, _ := leaf1.ToTx()

		_, hasRoot := nonces[rootTx.TxHash()]
		_, hasLeaf1 := nonces[leaf1Tx.TxHash()]

		require.True(t, hasRoot)
		require.True(t, hasLeaf1)
	})

	t.Run("single leaf signer path", func(t *testing.T) {
		t.Parallel()
		privKey, pubKey := createTestKey(t)

		leaf := createSimpleLeaf("leaf", 1000, []*btcec.PublicKey{
			pubKey,
		})

		initialOut := &wire.TxOut{Value: 5000}
		fetcher, err := leaf.PrevOutputFetcher(initialOut)
		require.NoError(t, err)

		signer := newMockMuSig2Signer(privKey)
		keyDesc := &keychain.KeyDescriptor{PubKey: pubKey}
		sweepRoot := make([]byte, 32)

		session, err := NewSignerSession(
			signer, keyDesc, sweepRoot, fetcher, leaf,
		)
		require.NoError(t, err)

		// Should have exactly 1 tx (the leaf).
		require.Len(t, session.txs, 1)

		nonces := session.GetNonces()
		require.Len(t, nonces, 1)

		leafTXID, _ := leaf.TXID()
		_, exists := nonces[leafTXID]
		require.True(t, exists)
	})
}

// TestTxSignerSessionSecurityNote tests the security documentation.
func TestTxSignerSessionSecurityNote(t *testing.T) {
	// This is more of a documentation test to verify the security
	// properties mentioned in the TxSignerSession comment.
	//
	// The comment states: "Each TxSignerSession automatically generates
	// fresh nonces via lnd's MuSig2 implementation. Do NOT reuse a
	// TxSignerSession for signing multiple transactions or re-signing the
	// same transaction, as this would constitute nonce reuse and leak the
	// private key."
	//
	// We verify this by ensuring:
	// 1. Nonces are generated automatically (not passed in)
	// 2. Each session gets unique nonces
	// 3. Documentation clearly warns against reuse

	t.Run("each session gets unique nonces", func(t *testing.T) {
		t.Parallel()
		privKey, pubKey := createTestKey(t)
		_, otherKey := createTestKey(t)

		signer := newMockMuSig2Signer(privKey)
		keyDesc := &keychain.KeyDescriptor{PubKey: pubKey}
		cosigners := []*btcec.PublicKey{pubKey, otherKey}
		sweepRoot := make([]byte, 32)

		// Create a node with the cosigners.
		node := createSimpleLeaf("test", 1000, cosigners)

		prevOut := &wire.TxOut{Value: 2000}
		fetcher := txscript.NewCannedPrevOutputFetcher(
			prevOut.PkScript, prevOut.Value,
		)

		// Create two sessions for same node (in real usage this is
		// unsafe!).
		session1, err := node.NewTxSignerSession(
			signer, sweepRoot, keyDesc, fetcher,
		)
		require.NoError(t, err)

		session2, err := node.NewTxSignerSession(
			signer, sweepRoot, keyDesc, fetcher,
		)
		require.NoError(t, err)

		// Nonces should be different (proves fresh generation).
		nonce1 := session1.GetNonce()
		nonce2 := session2.GetNonce()

		require.NotEqual(
			t, nonce1, nonce2,
			"Sessions should generate unique nonces",
		)
	})
}

// TestMusig2PubNonce tests the Musig2PubNonce type.
func TestMusig2PubNonce(t *testing.T) {
	t.Run("has correct size", func(t *testing.T) {
		var nonce Musig2PubNonce
		require.Equal(t, musig2.PubNonceSize, len(nonce))
		require.Equal(t, 66, len(nonce))
	})

	t.Run("can be used as map key", func(t *testing.T) {
		var nonce1, nonce2 Musig2PubNonce
		nonce1[0] = 1
		nonce2[0] = 2

		m := make(map[Musig2PubNonce]string)
		m[nonce1] = "first"
		m[nonce2] = "second"

		require.Equal(t, "first", m[nonce1])
		require.Equal(t, "second", m[nonce2])
	})

	t.Run("zero value is distinct", func(t *testing.T) {
		var zero Musig2PubNonce
		var nonZero Musig2PubNonce
		nonZero[0] = 1

		require.NotEqual(t, zero, nonZero)
		require.Equal(t, Musig2PubNonce{}, zero)
	})
}

// TestFullSigningFlow tests the complete signing workflow.
func TestFullSigningFlow(t *testing.T) {
	t.Run("complete 2-party signing for single transaction",
		func(t *testing.T) {
			t.Parallel()
			privKey1, pubKey1 := createTestKey(t)
			privKey2, pubKey2 := createTestKey(t)

			// Create a leaf that both parties sign.
			leaf := createSimpleLeaf(
				"leaf", 1000, []*btcec.PublicKey{
					pubKey1, pubKey2,
				},
			)

			// Compute FinalKey for verification later.
			sweepRoot := make([]byte, 32)
			finalKey, err := ComputeFinalKey(
				[]*btcec.PublicKey{pubKey1, pubKey2}, sweepRoot,
			)
			require.NoError(t, err)
			leaf.FinalKey = finalKey

			initialOut := &wire.TxOut{Value: 5000}
			fetcher, err := leaf.PrevOutputFetcher(initialOut)
			require.NoError(t, err)

			// Create signer sessions.
			signer1 := newMockMuSig2Signer(privKey1)
			signer2 := newMockMuSig2Signer(privKey2)

			keyDesc1 := &keychain.KeyDescriptor{PubKey: pubKey1}
			keyDesc2 := &keychain.KeyDescriptor{PubKey: pubKey2}

			session1, err := NewSignerSession(
				signer1, keyDesc1, sweepRoot, fetcher, leaf,
			)
			require.NoError(t, err)

			session2, err := NewSignerSession(
				signer2, keyDesc2, sweepRoot, fetcher, leaf,
			)
			require.NoError(t, err)

			// Phase 1: Exchange nonces.
			nonces1 := session1.GetNonces()
			nonces2 := session2.GetNonces()

			leafTXID, _ := leaf.TXID()

			// Phase 2: Aggregate and register nonces.
			aggNonce, err := musig2.AggregateNonces([][66]byte{
				nonces1[leafTXID], nonces2[leafTXID],
			})
			require.NoError(t, err)

			aggNonceSet := map[TxID]Musig2PubNonce{
				leafTXID: aggNonce,
			}
			err = session1.RegisterAggNonces(aggNonceSet)
			require.NoError(t, err)

			err = session2.RegisterAggNonces(aggNonceSet)
			require.NoError(t, err)

			// Phase 3: Generate partial signatures.
			partialSigs1, err := session1.Signatures(true)
			require.NoError(t, err)
			require.Len(t, partialSigs1, 1)
			require.NotNil(t, partialSigs1[leafTXID])

			partialSigs2, err := session2.Signatures(true)
			require.NoError(t, err)
			require.Len(t, partialSigs2, 1)
			require.NotNil(t, partialSigs2[leafTXID])

			// Note: In production, a coordinator would combine
			// these partial signatures. For this test, we verify
			// that the signing workflow completes successfully and
			// generates partial signatures from both parties.
		})

	t.Run("complete 2-party signing for tree with multiple txs",
		func(t *testing.T) {
			t.Parallel()
			privKey1, pubKey1 := createTestKey(t)
			privKey2, pubKey2 := createTestKey(t)

			// Create tree where both signers are in all
			// transactions.
			leaf1 := createSimpleLeaf(
				"leaf1", 1000, []*btcec.PublicKey{
					pubKey1, pubKey2,
				},
			)
			leaf2 := createSimpleLeaf(
				"leaf2", 2000, []*btcec.PublicKey{
					pubKey1, pubKey2,
				},
			)

			root := &Node{
				Input: wire.OutPoint{
					Hash:  chainhash.HashH([]byte("root")),
					Index: 0,
				},
				Outputs: []*wire.TxOut{
					{
						Value: 1000,
					},
					{
						Value: 2000,
					},
				},
				CoSigners: []*btcec.PublicKey{
					pubKey1,
					pubKey2,
				},
				Children: map[uint32]*Node{
					0: leaf1,
					1: leaf2,
				},
			}

			rootTXID, _ := root.TXID()
			leaf1.Input = wire.OutPoint{Hash: rootTXID, Index: 0}
			leaf2.Input = wire.OutPoint{Hash: rootTXID, Index: 1}

			// Compute FinalKey for all nodes.
			sweepRoot := make([]byte, 32)
			for node := range root.NodesIter() {
				finalKey, err := ComputeFinalKey(
					node.CoSigners, sweepRoot,
				)
				require.NoError(t, err)
				node.FinalKey = finalKey
			}

			initialOut := &wire.TxOut{Value: 5000}
			fetcher, err := root.PrevOutputFetcher(initialOut)
			require.NoError(t, err)

			// Create sessions.
			signer1 := newMockMuSig2Signer(privKey1)
			signer2 := newMockMuSig2Signer(privKey2)

			keyDesc1 := &keychain.KeyDescriptor{PubKey: pubKey1}
			keyDesc2 := &keychain.KeyDescriptor{PubKey: pubKey2}

			session1, err := NewSignerSession(
				signer1, keyDesc1, sweepRoot, fetcher, root,
			)
			require.NoError(t, err)

			session2, err := NewSignerSession(
				signer2, keyDesc2, sweepRoot, fetcher, root,
			)
			require.NoError(t, err)

			// Both should have 3 txs (root + 2 leaves).
			require.Len(t, session1.txs, 3)
			require.Len(t, session2.txs, 3)

			// Exchange nonces.
			nonces1 := session1.GetNonces()
			require.Len(t, nonces1, 3)

			nonces2 := session2.GetNonces()
			require.Len(t, nonces2, 3)

			// Aggregate nonces for all 3 transactions.
			aggNonceSet := make(map[TxID]Musig2PubNonce)
			for txid := range nonces1 {
				aggNonce, err := musig2.AggregateNonces(
					[][66]byte{
						nonces1[txid],
						nonces2[txid],
					},
				)
				require.NoError(t, err)
				aggNonceSet[txid] = aggNonce
			}

			// Register aggregated nonces.
			err = session1.RegisterAggNonces(aggNonceSet)
			require.NoError(t, err)

			err = session2.RegisterAggNonces(aggNonceSet)
			require.NoError(t, err)

			// Generate partial signatures for all txs.
			partialSigs1, err := session1.Signatures(true)
			require.NoError(t, err)
			require.Len(t, partialSigs1, 3)

			partialSigs2, err := session2.Signatures(true)
			require.NoError(t, err)
			require.Len(t, partialSigs2, 3)

			// Verify all partial signatures were generated.
			for txid := range session1.txs {
				require.NotNil(t, partialSigs1[txid])
				require.NotNil(t, partialSigs2[txid])
			}

			// Note: In production, a coordinator would combine
			// these partial signatures to create final
			// signatures.
		})
}
