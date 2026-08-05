package swaprpc

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
)

// TestOutSwapHTLCAckDigestBindsTerms verifies every accepted vHTLC term is
// committed by the acknowledgement digest.
func TestOutSwapHTLCAckDigestBindsTerms(t *testing.T) {
	t.Parallel()

	receiverKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	otherKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	paymentHash := lntypes.Hash{1, 2, 3}
	amountSat := uint64(42_000)
	pkScript := []byte{0x51, 0x20, 0x01}
	digest := OutSwapHTLCAckDigest(
		receiverKey.PubKey(), paymentHash, amountSat, pkScript,
	)

	tests := []struct {
		name        string
		receiverKey *btcec.PublicKey
		paymentHash lntypes.Hash
		amountSat   uint64
		pkScript    []byte
	}{
		{
			name:        "receiver key",
			receiverKey: otherKey.PubKey(),
			paymentHash: paymentHash,
			amountSat:   amountSat,
			pkScript:    pkScript,
		},
		{
			name:        "payment hash",
			receiverKey: receiverKey.PubKey(),
			paymentHash: lntypes.Hash{
				4,
				5,
				6,
			},
			amountSat: amountSat,
			pkScript:  pkScript,
		},
		{
			name:        "amount",
			receiverKey: receiverKey.PubKey(),
			paymentHash: paymentHash,
			amountSat:   amountSat + 1,
			pkScript:    pkScript,
		},
		{
			name:        "pkScript",
			receiverKey: receiverKey.PubKey(),
			paymentHash: paymentHash,
			amountSat:   amountSat,
			pkScript: []byte{
				0x51,
				0x20,
				0x02,
			},
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			mutated := OutSwapHTLCAckDigest(
				test.receiverKey, test.paymentHash,
				test.amountSat, test.pkScript,
			)
			require.NotEqual(t, digest, mutated)
		})
	}
}

// TestOutSwapHTLCAckDigestSignature verifies the canonical digest can be
// signed and verified with the receiver identity key.
func TestOutSwapHTLCAckDigestSignature(t *testing.T) {
	t.Parallel()

	receiverKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	digest := OutSwapHTLCAckDigest(
		receiverKey.PubKey(), lntypes.Hash{1}, 42_000,
		[]byte{0x51, 0x20, 0x01},
	)
	sig, err := schnorr.Sign(receiverKey, digest[:])
	require.NoError(t, err)
	require.True(t, sig.Verify(digest[:], receiverKey.PubKey()))
}
