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

	prevFetcher := txscript.NewCannedPrevOutputFetcher(
		pkScript, int64(inputValue),
	)
	sigHashes := txscript.NewTxSigHashes(sweepTx, prevFetcher)

	engine, err := txscript.NewEngine(
		pkScript, sweepTx, 0, txscript.StandardVerifyFlags, nil,
		sigHashes, int64(inputValue), prevFetcher,
	)
	require.NoError(t, err)
	require.NoError(t, engine.Execute())
}

// TestBuildSignedSweepTxBatchRoot verifies that sweeping a real batch root
// output succeeds when the internal key is a MuSig2 aggregate over the
// operator and client signing keys.
func TestBuildSignedSweepTxBatchRoot(t *testing.T) {
	t.Parallel()

	operatorKey, _ := testutils.CreateKey(10)
	sweepPubKey, signer := testutils.CreateKey(11)
	client1Owner, _ := testutils.CreateKey(12)
	client1Signing, _ := testutils.CreateKey(13)
	client2Owner, _ := testutils.CreateKey(14)
	client2Signing, _ := testutils.CreateKey(15)

	sweepDelay := uint32(10)

	vtxo1, err := treepkg.NewVTXODescriptor(
		btcutil.Amount(120_000), client1Owner, operatorKey,
		client1Signing, sweepDelay,
	)
	require.NoError(t, err)

	vtxo2, err := treepkg.NewVTXODescriptor(
		btcutil.Amount(80_000), client2Owner, operatorKey,
		client2Signing, sweepDelay,
	)
	require.NoError(t, err)

	vtxos := []treepkg.VTXODescriptor{*vtxo1, *vtxo2}

	batchOutput, err := treepkg.BuildBatchOutput(
		vtxos, operatorKey, sweepPubKey, sweepDelay,
	)
	require.NoError(t, err)

	batchOutpoint := wire.OutPoint{
		Hash:  chainhash.Hash{2},
		Index: 0,
	}

	tree, err := treepkg.BuildVTXOTree(
		batchOutpoint, batchOutput, vtxos, operatorKey,
		sweepPubKey, sweepDelay, 2,
	)
	require.NoError(t, err)

	candidates := []*batchwatcher.Output{{
		Outpoint: batchOutpoint,
		TxOut:    batchOutput,
		TreeNode: tree.Root,
	}}

	sweepTx, err := buildSignedSweepTx(
		candidates, keychain.KeyDescriptor{PubKey: sweepPubKey},
		sweepDelay, []byte{0x51}, btcutil.Amount(1), signer,
	)
	require.NoError(t, err)

	prevFetcher := txscript.NewCannedPrevOutputFetcher(
		batchOutput.PkScript, batchOutput.Value,
	)
	sigHashes := txscript.NewTxSigHashes(sweepTx, prevFetcher)

	engine, err := txscript.NewEngine(
		batchOutput.PkScript, sweepTx, 0, txscript.StandardVerifyFlags,
		nil, sigHashes, batchOutput.Value, prevFetcher,
	)
	require.NoError(t, err)
	require.NoError(t, engine.Execute())
}
