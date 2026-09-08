package waved

import (
	"testing"

	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/lightninglabs/wavelength/internal/expiryfixture"
	libtypes "github.com/lightninglabs/wavelength/lib/types"
	"github.com/stretchr/testify/require"
)

// TestRecoveryDescriptorFromIndexerRebuildsIndexerScript verifies a recovered
// descriptor's tapscript hashes back to the pk script the operator holds when
// the indexer's relative_expiry differs from the operator's VTXO exit delay.
func TestRecoveryDescriptorFromIndexerRebuildsIndexerScript(t *testing.T) {
	t.Parallel()

	clientKey := testKeyDescriptor(t, 210)
	operatorKey := testKeyDescriptor(t, 211)
	terms := &libtypes.OperatorTerms{
		PubKey:        operatorKey.PubKey,
		VTXOExitDelay: recoveryTestExitDelay,
	}

	pkScript, err := BuildPubKeyVTXOReceiveScript(
		clientKey.PubKey, terms.PubKey, terms.VTXOExitDelay,
	)
	require.NoError(t, err)

	const batchRelativeExpiry = 1008
	require.NotEqual(
		t, uint32(batchRelativeExpiry), terms.VTXOExitDelay,
	)

	indexed, _ := expiryfixture.Round(
		t, 28674, pkScript, batchRelativeExpiry, 964273, 1,
	)
	indexed.BatchExpiryHeight = 965281
	indexed.RelativeExpiry = batchRelativeExpiry

	desc, ok, err := recoveryDescriptorFromIndexer(
		indexed, clientKey, terms,
	)
	require.NoError(t, err)
	require.True(t, ok)

	// The same check the operator runs when validating a join request.
	tapKey, err := desc.TapScript.TaprootKey()
	require.NoError(t, err)

	rebuilt, err := txscript.PayToTaprootScript(tapKey)
	require.NoError(t, err)
	require.Equal(t, desc.PkScript, rebuilt)
	require.Equal(t, pkScript, rebuilt)

	require.Equal(t, terms.VTXOExitDelay, desc.RelativeExpiry)
}
