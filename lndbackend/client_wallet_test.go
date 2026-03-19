package lndbackend

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
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
