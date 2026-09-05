package round

import (
	"crypto/rand"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/lib/tree"
	"github.com/stretchr/testify/require"
)

// TestValidateRoundSweepPolicy verifies the advertised key and delay against
// every committed VTXO-tree sweep root.
func TestValidateRoundSweepPolicy(t *testing.T) {
	t.Parallel()

	sweepKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	otherKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	const sweepDelay = uint32(1008)
	leaf, err := arkscript.UnilateralCSVTimeoutTapLeaf(
		sweepKey.PubKey(), sweepDelay,
	)
	require.NoError(t, err)
	root := leaf.TapHash()

	trees := map[int]*tree.Tree{
		0: {
			SweepTapscriptRoot: root[:],
		},
		1: {
			SweepTapscriptRoot: root[:],
		},
	}
	require.NoError(
		t,
		validateRoundSweepPolicy(
			sweepKey.PubKey(), sweepDelay, trees,
		),
	)

	require.ErrorContains(
		t, validateRoundSweepPolicy(
			nil, sweepDelay, trees,
		),
		"sweep key must be provided",
	)
	require.ErrorContains(
		t,
		validateRoundSweepPolicy(
			sweepKey.PubKey(), 0, trees,
		),
		"sweep delay must be positive",
	)
	require.ErrorContains(
		t,
		validateRoundSweepPolicy(
			otherKey.PubKey(), sweepDelay, trees,
		),
		"does not match",
	)

	randomRoot := make([]byte, len(root))
	_, err = rand.Read(randomRoot)
	require.NoError(t, err)
	trees[1] = &tree.Tree{SweepTapscriptRoot: randomRoot}
	require.ErrorContains(
		t,
		validateRoundSweepPolicy(
			sweepKey.PubKey(), sweepDelay, trees,
		),
		"output 1",
	)
}
