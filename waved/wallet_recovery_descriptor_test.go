package waved

import (
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/lightninglabs/wavelength/arkrpc"
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

	commitmentTxID := chainhash.Hash{0xcc}
	outpointTxID := chainhash.Hash{0xdd}
	indexed := &arkrpc.VTXO{
		Outpoint: &arkrpc.OutPoint{
			Txid: outpointTxID[:],
			Vout: 0,
		},
		ValueSat:          28674,
		PkScript:          pkScript,
		Status:            arkrpc.VTXOStatus_VTXO_STATUS_LIVE,
		RoundId:           "round-1",
		CommitmentTxid:    commitmentTxID[:],
		CreatedHeight:     964273,
		BatchExpiryHeight: 965281,
		RelativeExpiry:    batchRelativeExpiry,
		AncestryPaths: []*arkrpc.AncestryPath{
			recoveryTestAncestryPath(t, commitmentTxID),
		},
	}

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
