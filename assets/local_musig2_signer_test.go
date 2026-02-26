package assets

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// testPrivKey generates a deterministic private key for testing.
func testPrivKey(t *testing.T, seed byte) *btcec.PrivateKey {
	t.Helper()

	var privKeyBytes [32]byte
	for i := range privKeyBytes {
		privKeyBytes[i] = seed
	}

	privKey, _ := btcec.PrivKeyFromBytes(privKeyBytes[:])

	return privKey
}

// TestLocalMuSig2SignerSessionLifecycle tests the full signing flow.
func TestLocalMuSig2SignerSessionLifecycle(t *testing.T) {
	// Create two signers.
	privKey1 := testPrivKey(t, 0x01)
	privKey2 := testPrivKey(t, 0x02)

	signer1 := NewLocalMuSig2Signer(privKey1)
	signer2 := NewLocalMuSig2Signer(privKey2)

	allPubKeys := []*btcec.PublicKey{
		privKey1.PubKey(),
		privKey2.PubKey(),
	}

	// Create sessions with BIP86 tweak.
	tweaks := &input.MuSig2Tweaks{
		TaprootBIP0086Tweak: true,
	}

	session1, err := signer1.MuSig2CreateSession(
		input.MuSig2Version100RC2, keychain.KeyLocator{},
		allPubKeys, tweaks, nil, nil,
	)
	require.NoError(t, err)
	require.NotNil(t, session1)
	require.NotEmpty(t, session1.SessionID)

	session2, err := signer2.MuSig2CreateSession(
		input.MuSig2Version100RC2, keychain.KeyLocator{},
		allPubKeys, tweaks, nil, nil,
	)
	require.NoError(t, err)
	require.NotNil(t, session2)

	// Exchange nonces.
	haveAll1, err := signer1.MuSig2RegisterNonces(
		session1.SessionID,
		[][musig2.PubNonceSize]byte{session2.PublicNonce},
	)
	require.NoError(t, err)
	require.True(t, haveAll1)

	haveAll2, err := signer2.MuSig2RegisterNonces(
		session2.SessionID,
		[][musig2.PubNonceSize]byte{session1.PublicNonce},
	)
	require.NoError(t, err)
	require.True(t, haveAll2)

	// Sign a message.
	message := [32]byte{0xde, 0xad, 0xbe, 0xef}

	partialSig1, err := signer1.MuSig2Sign(
		session1.SessionID, message, false,
	)
	require.NoError(t, err)
	require.NotNil(t, partialSig1)

	partialSig2, err := signer2.MuSig2Sign(
		session2.SessionID, message, false,
	)
	require.NoError(t, err)
	require.NotNil(t, partialSig2)

	// Combine signatures.
	finalSig, haveAll, err := signer1.MuSig2CombineSig(
		session1.SessionID,
		[]*musig2.PartialSignature{partialSig2},
	)
	require.NoError(t, err)
	require.True(t, haveAll)
	require.NotNil(t, finalSig)

	// Verify the signature is 64 bytes (Schnorr).
	require.Len(t, finalSig.Serialize(), 64)

	// Cleanup.
	err = signer1.MuSig2Cleanup(session1.SessionID)
	require.NoError(t, err)

	err = signer2.MuSig2Cleanup(session2.SessionID)
	require.NoError(t, err)
}

// TestLocalMuSig2SignerWithTaprootTweak tests signing with a taproot tweak.
func TestLocalMuSig2SignerWithTaprootTweak(t *testing.T) {
	privKey1 := testPrivKey(t, 0x03)
	privKey2 := testPrivKey(t, 0x04)

	signer1 := NewLocalMuSig2Signer(privKey1)
	signer2 := NewLocalMuSig2Signer(privKey2)

	allPubKeys := []*btcec.PublicKey{
		privKey1.PubKey(),
		privKey2.PubKey(),
	}

	// Create a taproot tweak (simulating a merkle root).
	taprootTweak := [32]byte{0x01, 0x02, 0x03}

	tweaks := &input.MuSig2Tweaks{
		TaprootTweak: taprootTweak[:],
	}

	session1, err := signer1.MuSig2CreateSession(
		input.MuSig2Version100RC2, keychain.KeyLocator{},
		allPubKeys, tweaks, nil, nil,
	)
	require.NoError(t, err)

	session2, err := signer2.MuSig2CreateSession(
		input.MuSig2Version100RC2, keychain.KeyLocator{},
		allPubKeys, tweaks, nil, nil,
	)
	require.NoError(t, err)

	// Combined keys should match.
	require.Equal(t,
		session1.CombinedKey.SerializeCompressed(),
		session2.CombinedKey.SerializeCompressed(),
	)

	// The final key should be different from untweaked.
	untweakedSession, err := signer1.MuSig2CreateSession(
		input.MuSig2Version100RC2, keychain.KeyLocator{},
		allPubKeys, &input.MuSig2Tweaks{}, nil, nil,
	)
	require.NoError(t, err)

	require.NotEqual(t,
		session1.CombinedKey.SerializeCompressed(),
		untweakedSession.CombinedKey.SerializeCompressed(),
	)
}

// TestLocalMuSig2SignerInvalidSession tests error handling for invalid
// sessions.
func TestLocalMuSig2SignerInvalidSession(t *testing.T) {
	privKey := testPrivKey(t, 0x05)
	signer := NewLocalMuSig2Signer(privKey)

	invalidID := [32]byte{0xff}

	// RegisterNonces with invalid session.
	_, err := signer.MuSig2RegisterNonces(invalidID, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "not found")

	// Sign with invalid session.
	_, err = signer.MuSig2Sign(invalidID, [32]byte{}, false)
	require.Error(t, err)
	require.Contains(t, err.Error(), "not found")

	// CombineSig with invalid session.
	_, _, err = signer.MuSig2CombineSig(invalidID, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "not found")

	// Cleanup with invalid session should not error (idempotent).
	err = signer.MuSig2Cleanup(invalidID)
	require.NoError(t, err)
}

// TestLocalMuSig2SignerMultipleSessions tests managing multiple concurrent
// sessions.
func TestLocalMuSig2SignerMultipleSessions(t *testing.T) {
	privKey := testPrivKey(t, 0x06)
	signer := NewLocalMuSig2Signer(privKey)

	allPubKeys := []*btcec.PublicKey{privKey.PubKey()}

	// Create multiple sessions.
	session1, err := signer.MuSig2CreateSession(
		input.MuSig2Version100RC2, keychain.KeyLocator{},
		allPubKeys, &input.MuSig2Tweaks{}, nil, nil,
	)
	require.NoError(t, err)

	session2, err := signer.MuSig2CreateSession(
		input.MuSig2Version100RC2, keychain.KeyLocator{},
		allPubKeys, &input.MuSig2Tweaks{}, nil, nil,
	)
	require.NoError(t, err)

	// Sessions should have different IDs.
	require.NotEqual(t, session1.SessionID, session2.SessionID)

	// Clean up one session.
	err = signer.MuSig2Cleanup(session1.SessionID)
	require.NoError(t, err)

	// Second session should still work.
	_, err = signer.MuSig2RegisterNonces(session2.SessionID, nil)
	require.NoError(t, err)
}
