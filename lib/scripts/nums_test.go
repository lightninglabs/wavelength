package scripts

import (
	"crypto/sha256"
	"encoding/binary"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/stretchr/testify/require"
)

// TestARKNUMSKeyValid tests that the ARKNUMSKey is valid.
func TestARKNUMSKeyValid(t *testing.T) {
	t.Parallel()

	require.NotNil(t, ARKNUMSKey)

	// Verify it's actually a valid point on the curve
	require.True(t, ARKNUMSKey.IsOnCurve())
}

// TestARKNUMSKeyDerivation tests that the ARKNUMSKey can be derived
// using the logic defined in
// https://github.com/lightninglabs/lightning-node-connect/tree/
// master/mailbox/numsgen, with the seed phrase "Ark Protocol NUMS".
func TestARKNUMSKeyDerivation(t *testing.T) {
	t.Parallel()

	// increment is a helper function that derives the candidate
	// NUMS key given an iteration index.
	increment := func(i int) []byte {
		// Increment the origin of the hash using the mapping
		// candidateSed := h(i || seedPhrase).
		shaStream := sha256.New()

		var iterationBytes [8]byte
		binary.BigEndian.PutUint64(iterationBytes[:], uint64(i))
		shaStream.Write(iterationBytes[:])

		shaStream.Write([]byte(ARKNUMSSeedPhrase))

		seedHash := shaStream.Sum(nil)

		return seedHash
	}

	candidateSeed := increment(0)

	var pubBytes [33]byte
	pubBytes[0] = 0x02
	copy(pubBytes[1:], candidateSeed)

	candidatePoint, err := btcec.ParsePubKey(pubBytes[:])
	require.NoError(t, err)
	require.True(t, candidatePoint.IsEqual(&ARKNUMSKey))
}
