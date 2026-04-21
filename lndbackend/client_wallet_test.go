package lndbackend

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/stretchr/testify/require"
)

func TestSerializeMuSig2SignerPubKeys(t *testing.T) {
	t.Parallel()

	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	pubKey := privKey.PubKey()
	signers := []*btcec.PublicKey{pubKey}

	t.Run("v040 uses x-only keys", func(t *testing.T) {
		t.Parallel()

		got := serializeMuSig2SignerPubKeys(
			input.MuSig2Version040, signers,
		)

		require.Len(t, got, 1)
		require.Len(t, got[0], schnorr.PubKeyBytesLen)
		require.Equal(t, schnorr.SerializePubKey(pubKey), got[0])
	})

	t.Run("v100rc2 uses compressed keys", func(t *testing.T) {
		t.Parallel()

		got := serializeMuSig2SignerPubKeys(
			input.MuSig2Version100RC2, signers,
		)

		require.Len(t, got, 1)
		require.Len(t, got[0], btcec.PubKeyBytesLenCompressed)
		require.Equal(t, pubKey.SerializeCompressed(), got[0])
	})
}

func TestDerivePeerNonce(t *testing.T) {
	t.Parallel()

	localPrivKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	peerPrivKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	localNonces, err := musig2.GenNonces(
		musig2.WithPublicKey(localPrivKey.PubKey()),
		musig2.WithNonceSecretKeyAux(localPrivKey),
	)
	require.NoError(t, err)

	peerNonces, err := musig2.GenNonces(
		musig2.WithPublicKey(peerPrivKey.PubKey()),
		musig2.WithNonceSecretKeyAux(peerPrivKey),
	)
	require.NoError(t, err)

	combinedNonce, err := musig2.AggregateNonces(
		[][musig2.PubNonceSize]byte{
			localNonces.PubNonce,
			peerNonces.PubNonce,
		},
	)
	require.NoError(t, err)

	derivedPeerNonce, err := derivePeerNonce(
		combinedNonce, localNonces.PubNonce,
	)
	require.NoError(t, err)
	require.Equal(t, peerNonces.PubNonce, derivedPeerNonce)
}
