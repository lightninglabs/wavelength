package tapassets

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/stretchr/testify/require"
)

// TestComposedBoardingScript proves the hash-only recompute reproduces
// the script txscript derives when it is handed the same tree, and that
// a different commitment leaf hash produces a different script.
func TestComposedBoardingScript(t *testing.T) {
	t.Parallel()

	owner, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	operator, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	template, err := arkscript.EncodeStandardVTXOTemplate(
		owner.PubKey(), operator.PubKey(), 144,
	)
	require.NoError(t, err)

	leafHash := [32]byte(chainhash.HashH([]byte("commitment leaf")))

	pkScript, internalKey, composedRoot, err := ComposedBoardingScript(
		template, leafHash,
	)
	require.NoError(t, err)
	require.NotNil(t, internalKey)

	// Independent derivation through txscript primitives.
	decoded, err := arkscript.DecodePolicyTemplate(template)
	require.NoError(t, err)
	policy, err := decoded.Compile()
	require.NoError(t, err)

	left, right := [32]byte(policy.RootHash), leafHash
	if string(right[:]) < string(left[:]) {
		left, right = right, left
	}
	root := chainhash.TaggedHash(
		chainhash.TagTapBranch, left[:], right[:],
	)
	wantKey := txscript.ComputeTaprootOutputKey(
		policy.InternalKey, root[:],
	)
	wantScript, err := txscript.PayToTaprootScript(wantKey)
	require.NoError(t, err)

	require.Equal(t, wantScript, pkScript)
	require.Equal(t, [32]byte(*root), composedRoot)

	// A different disclosed hash cannot reproduce the same script.
	otherHash := [32]byte(chainhash.HashH([]byte("other leaf")))
	otherScript, _, _, err := ComposedBoardingScript(
		template, otherHash,
	)
	require.NoError(t, err)
	require.NotEqual(t, pkScript, otherScript)
}
