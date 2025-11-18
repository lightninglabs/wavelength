package assets

import (
	"crypto/rand"
	"crypto/sha256"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/txscript"
	"github.com/lightningnetwork/lnd/input"
	"github.com/stretchr/testify/require"
)

// TestMuSig2Signer tests the basic MuSig2 signing flow.
func TestMuSig2Signer(t *testing.T) {
	t.Parallel()

	// Generate test keys for user and operator.
	userPrivKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	operatorPrivKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	// Create a test message to sign.
	message := sha256.Sum256([]byte("test message"))

	// Create signers for both parties.
	userSigner, err := NewMuSig2Signer(
		userPrivKey,
		[]*btcec.PublicKey{operatorPrivKey.PubKey()},
		nil, // No taproot tweak.
	)
	require.NoError(t, err)
	require.NotNil(t, userSigner)

	operatorSigner, err := NewMuSig2Signer(
		operatorPrivKey,
		[]*btcec.PublicKey{userPrivKey.PubKey()},
		nil, // No taproot tweak.
	)
	require.NoError(t, err)
	require.NotNil(t, operatorSigner)

	// Exchange nonces.
	userNonce := userSigner.PublicNonce()
	operatorNonce := operatorSigner.PublicNonce()

	err = userSigner.ReceiveNonce(operatorPrivKey.PubKey(), operatorNonce)
	require.NoError(t, err)

	err = operatorSigner.ReceiveNonce(userPrivKey.PubKey(), userNonce)
	require.NoError(t, err)

	// Both parties create partial signatures.
	userPartialSig, err := userSigner.Sign(message)
	require.NoError(t, err)
	require.NotNil(t, userPartialSig)

	operatorPartialSig, err := operatorSigner.Sign(message)
	require.NoError(t, err)
	require.NotNil(t, operatorPartialSig)

	// Combine signatures (either party can do this).
	finalSig, err := userSigner.CombineSignatures(
		message,
		[]*musig2.PartialSignature{userPartialSig, operatorPartialSig},
	)
	require.NoError(t, err)
	require.NotNil(t, finalSig)

	// Verify signature.
	// Compute aggregate key for verification.
	sortKeys := true
	aggregateKey, err := input.MuSig2CombineKeys(
		input.MuSig2Version100RC2,
		[]*btcec.PublicKey{
			userPrivKey.PubKey(), operatorPrivKey.PubKey(),
		},
		sortKeys,
		&input.MuSig2Tweaks{},
	)
	require.NoError(t, err)

	// Verify the signature against the aggregate key.
	valid := finalSig.Verify(message[:], aggregateKey.FinalKey)
	require.True(t, valid, "signature should be valid")
}

// TestMuSig2SignerWithTaprootTweak tests MuSig2 signing with a taproot tweak.
func TestMuSig2SignerWithTaprootTweak(t *testing.T) {
	t.Parallel()

	// Generate test keys for user and operator.
	userPrivKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	operatorPrivKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	// Create a test taproot tweak (32 bytes).
	taprootTweak := make([]byte, 32)
	_, err = rand.Read(taprootTweak)
	require.NoError(t, err)

	// Create a test message to sign.
	message := sha256.Sum256([]byte("test message with taproot tweak"))

	// Create signers for both parties with taproot tweak.
	userSigner, err := NewMuSig2Signer(
		userPrivKey,
		[]*btcec.PublicKey{operatorPrivKey.PubKey()},
		taprootTweak,
	)
	require.NoError(t, err)

	operatorSigner, err := NewMuSig2Signer(
		operatorPrivKey,
		[]*btcec.PublicKey{userPrivKey.PubKey()},
		taprootTweak,
	)
	require.NoError(t, err)

	// Exchange nonces.
	userNonce := userSigner.PublicNonce()
	operatorNonce := operatorSigner.PublicNonce()

	err = userSigner.ReceiveNonce(operatorPrivKey.PubKey(), operatorNonce)
	require.NoError(t, err)

	err = operatorSigner.ReceiveNonce(userPrivKey.PubKey(), userNonce)
	require.NoError(t, err)

	// Both parties create partial signatures.
	userPartialSig, err := userSigner.Sign(message)
	require.NoError(t, err)

	operatorPartialSig, err := operatorSigner.Sign(message)
	require.NoError(t, err)

	// Combine signatures.
	finalSig, err := userSigner.CombineSignatures(
		message,
		[]*musig2.PartialSignature{userPartialSig, operatorPartialSig},
	)
	require.NoError(t, err)

	// Verify signature with tweaked key.
	// Compute aggregate key.
	sortKeys := true
	aggregateKey, err := input.MuSig2CombineKeys(
		input.MuSig2Version100RC2,
		[]*btcec.PublicKey{
			userPrivKey.PubKey(), operatorPrivKey.PubKey(),
		},
		sortKeys,
		&input.MuSig2Tweaks{},
	)
	require.NoError(t, err)

	// Apply taproot tweak to get final key.
	tweakedKey := txscript.ComputeTaprootOutputKey(
		aggregateKey.PreTweakedKey,
		taprootTweak,
	)

	// Strip parity for verification.
	tweakedKey, _ = schnorr.ParsePubKey(schnorr.SerializePubKey(tweakedKey))

	// Verify the signature against the tweaked key.
	valid := finalSig.Verify(message[:], tweakedKey)
	require.True(t, valid, "signature should be valid with taproot tweak")
}

// TestMuSig2SignerErrorHandling tests error cases.
func TestMuSig2SignerErrorHandling(t *testing.T) {
	t.Parallel()

	userPrivKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	operatorPrivKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	message := sha256.Sum256([]byte("test"))

	// Test signing without receiving nonce.
	signer, err := NewMuSig2Signer(
		userPrivKey,
		[]*btcec.PublicKey{operatorPrivKey.PubKey()},
		nil,
	)
	require.NoError(t, err)

	_, err = signer.Sign(message)
	require.Error(t, err)
	require.Contains(t, err.Error(), "must receive all")

	// Test combining signatures without nonce.
	_, err = signer.CombineSignatures(message, nil)
	require.Error(t, err)
	require.Contains(
		t, err.Error(), "must receive all cosigner nonces first",
	)

	// Test receiving nonce twice.
	otherSigner, err := NewMuSig2Signer(
		operatorPrivKey,
		[]*btcec.PublicKey{userPrivKey.PubKey()},
		nil,
	)
	require.NoError(t, err)

	otherNonce := otherSigner.PublicNonce()
	err = signer.ReceiveNonce(operatorPrivKey.PubKey(), otherNonce)
	require.NoError(t, err)

	err = signer.ReceiveNonce(operatorPrivKey.PubKey(), otherNonce)
	require.Error(t, err)
	require.Contains(t, err.Error(), "nonce already received")
}
