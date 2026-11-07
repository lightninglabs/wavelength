package arkscript

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/stretchr/testify/require"
)

// TestComposedBoardingExitSpend checks that the exit spend of a deposit output
// binds to the composed script and not to the plain boarding output.
func TestComposedBoardingExitSpend(t *testing.T) {
	t.Parallel()

	_, owner := btcec.PrivKeyFromBytes([]byte{0x01})
	_, operator := btcec.PrivKeyFromBytes([]byte{0x02})
	leafHash := [32]byte{0xaa, 0xbb}

	template, err := EncodeStandardVTXOTemplate(owner, operator, 144)
	require.NoError(t, err)
	composed, _, _, err := ComposedBoardingScript(template, leafHash)
	require.NoError(t, err)
	_, plain, err := EncodeStandardVTXOArtifacts(owner, operator, 144)
	require.NoError(t, err)

	spend, err := ComposedBoardingExitSpend(owner, operator, 144, leafHash)
	require.NoError(t, err)
	require.NoError(t, spend.Validate())
	require.NoError(t, spend.VerifyBindsToPkScript(composed))
	require.Error(t, spend.VerifyBindsToPkScript(plain))
	require.EqualValues(t, 144, spend.RequiredSequence)

	exit, err := NewVTXOSpendInfoFromPolicy(owner, operator, 144, 1)
	require.NoError(t, err)
	require.Equal(t, exit.WitnessScript, spend.WitnessScript)

	other, err := ComposedBoardingExitSpend(
		owner, operator, 144, [32]byte{0xcc},
	)
	require.NoError(t, err)
	require.Error(t, other.VerifyBindsToPkScript(composed))
}
