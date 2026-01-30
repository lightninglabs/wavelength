package batchsweeper

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/mempool"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/scripts"
	treepkg "github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo/batchwatcher"
	"github.com/lightninglabs/darepo/internal/testutils"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// TestBuildSignedSweepTx verifies that buildSignedSweepTx can construct and
// sign a sweep transaction for a single operator-controlled output.
func TestBuildSignedSweepTx(t *testing.T) {
	t.Parallel()

	internalKey, _ := testutils.CreateKey(1)
	sweepPubKey, signer := testutils.CreateKey(2)

	sweepDelay := uint32(10)

	sweepLeaf, err := scripts.UnilateralCSVTimeoutTapLeaf(
		sweepPubKey, sweepDelay,
	)
	require.NoError(t, err)

	tapTree := txscript.AssembleTaprootScriptTree(sweepLeaf)
	rootHash := tapTree.RootNode.TapHash()
	outputKey := txscript.ComputeTaprootOutputKey(
		internalKey, rootHash[:],
	)

	pkScript, err := txscript.PayToTaprootScript(outputKey)
	require.NoError(t, err)

	outpoint := wire.OutPoint{
		Hash:  chainhash.Hash{1},
		Index: 0,
	}

	inputValue := btcutil.Amount(100_000)
	txOut := wire.NewTxOut(int64(inputValue), pkScript)

	node := &treepkg.Node{
		CoSigners: []*btcec.PublicKey{internalKey},
	}

	candidates := []*batchwatcher.Output{
		{
			Outpoint:    outpoint,
			TxOut:       txOut,
			IsVTXO:      false,
			TreeNode:    node,
			OutputIndex: 0,
		},
	}

	sweepKey := keychain.KeyDescriptor{
		PubKey: sweepPubKey,
	}

	sweepTx, err := buildSignedSweepTx(
		candidates, sweepKey, sweepDelay, []byte{0x51},
		btcutil.Amount(1), signer,
	)
	require.NoError(t, err)
	require.NotNil(t, sweepTx)

	require.EqualValues(t, 2, sweepTx.Version)
	require.Len(t, sweepTx.TxIn, 1)
	require.Equal(t, outpoint, sweepTx.TxIn[0].PreviousOutPoint)
	require.EqualValues(t, sweepDelay, sweepTx.TxIn[0].Sequence)
	require.NotEmpty(t, sweepTx.TxIn[0].Witness)
	require.Len(t, sweepTx.TxOut, 1)

	vsize := mempool.GetTxVirtualSize(btcutil.NewTx(sweepTx))
	expectedFee := btcutil.Amount(vsize)

	require.EqualValues(t,
		int64(inputValue-expectedFee),
		sweepTx.TxOut[0].Value,
	)
}
