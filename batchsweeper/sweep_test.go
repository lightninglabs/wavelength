package batchsweeper

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/mempool"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	treepkg "github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo/batchwatcher"
	"github.com/lightninglabs/darepo/internal/testutils"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// strictWitnessSigner wraps an input.Signer and asserts that, on every
// SignOutputRaw call, none of the tx inputs carry leftover witness data
// from a prior signing pass. This mimics the failure mode that lndclient
// + a remote-signer LND surface in production: lndclient's encodeTx
// serializes the witness, the watch-only LND can't wrap a witness-bearing
// tx in a fresh PSBT (psbt.NewFromUnsignedTx rejects it), and the signer
// silently returns no TaprootScriptSpendSig (manifesting as
// "remote signer returned invalid taproot script spend signature, wanted
// 1, got 0").
//
// The mock signer that backs CreateKey computes the sighash in-process
// and ignores leftover witness, so without this wrapper a test using
// MockSigner cannot tell the difference between a pre-fix and a post-fix
// buildSignedSweepTx.
type strictWitnessSigner struct {
	input.Signer
	t *testing.T
}

func (s *strictWitnessSigner) SignOutputRaw(tx *wire.MsgTx,
	signDesc *input.SignDescriptor) (input.Signature, error) {

	s.t.Helper()
	for i, in := range tx.TxIn {
		require.Empty(s.t, in.Witness, "input %d carries leftover "+
			"witness from a prior signing pass; this would fail "+
			"in production via lndclient → remote-signer LND", i)
	}

	return s.Signer.SignOutputRaw(tx, signDesc)
}

// TestBuildSignedSweepTx verifies that buildSignedSweepTx can construct and
// sign a sweep transaction for a single operator-controlled output.
func TestBuildSignedSweepTx(t *testing.T) {
	t.Parallel()

	internalKey, _ := testutils.CreateKey(1)
	sweepPubKey, signer := testutils.CreateKey(2)

	sweepDelay := uint32(10)

	sweepLeaf, err := arkscript.UnilateralCSVTimeoutTapLeaf(
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

// TestBuildSignedSweepTxClearsWitnessBetweenPasses is a regression test for
// the BSWP signing failure where the second signing pass forwarded a tx
// with the first pass's witness still attached to lndclient's encodeTx.
// The watch-only LND would then reject the witness-bearing tx silently,
// surfacing as "remote signer returned invalid taproot script spend
// signature, wanted 1, got 0".
//
// Wrapping the mock signer in strictWitnessSigner lets us catch the bug
// in a unit test: every SignOutputRaw invocation asserts no input has
// leftover witness data, so a regression in signSweepInputs would fail
// the test deterministically.
func TestBuildSignedSweepTxClearsWitnessBetweenPasses(t *testing.T) {
	t.Parallel()

	internalKey, _ := testutils.CreateKey(20)
	sweepPubKey, mockSigner := testutils.CreateKey(21)
	signer := &strictWitnessSigner{Signer: mockSigner, t: t}

	sweepDelay := uint32(10)

	sweepLeaf, err := arkscript.UnilateralCSVTimeoutTapLeaf(
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

	inputValue := btcutil.Amount(100_000)
	candidates := []*batchwatcher.Output{{
		Outpoint: wire.OutPoint{Hash: chainhash.Hash{1}, Index: 0},
		TxOut:    wire.NewTxOut(int64(inputValue), pkScript),
		TreeNode: &treepkg.Node{
			CoSigners: []*btcec.PublicKey{internalKey},
		},
	}}

	// buildSignedSweepTx calls signSweepInputs twice. The strict wrapper
	// will fail the test if either call sees leftover witness on an
	// input.
	sweepTx, err := buildSignedSweepTx(
		candidates, keychain.KeyDescriptor{PubKey: sweepPubKey},
		sweepDelay, []byte{0x51}, btcutil.Amount(1), signer,
	)
	require.NoError(t, err)
	require.Len(t, sweepTx.TxIn, 1)
	require.NotEmpty(t, sweepTx.TxIn[0].Witness,
		"final tx should still have witness attached after the "+
			"second pass — the fix only clears it INSIDE "+
			"signSweepInputs before each pass starts")
}

// TestBuildSignedSweepTxClearsWitnessBetweenInputs is the multi-input
// counterpart to TestBuildSignedSweepTxClearsWitnessBetweenPasses.
//
// Even with the per-pass witness clear in place, the inner per-input
// loop assigns tx.TxIn[i].Witness immediately after each SignOutputRaw
// returns. On a multi-candidate sweep that means input N's signing call
// would see input N-1's witness still attached to the tx, which through
// lndclient → watch-only lnd would re-trigger the same silent
// PSBT-validation rejection ("wanted 1, got 0").
//
// strictWitnessSigner asserts on every SignOutputRaw that the tx is
// fully witness-free, so a regression that re-introduces inline witness
// assignment fails this test deterministically.
func TestBuildSignedSweepTxClearsWitnessBetweenInputs(t *testing.T) {
	t.Parallel()

	internalKey, _ := testutils.CreateKey(30)
	sweepPubKey, mockSigner := testutils.CreateKey(31)
	signer := &strictWitnessSigner{Signer: mockSigner, t: t}

	sweepDelay := uint32(10)

	sweepLeaf, err := arkscript.UnilateralCSVTimeoutTapLeaf(
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

	// Build three independent candidates so the inner loop runs more
	// than once per pass. All three reuse the same internal key and
	// sweep leaf to keep the test focused on the witness-clear
	// invariant rather than tree variation.
	const numInputs = 3
	candidates := make([]*batchwatcher.Output, numInputs)
	for i := 0; i < numInputs; i++ {
		hash := chainhash.Hash{}
		hash[0] = byte(i + 1)

		value := int64(100_000 + i*1_000)
		candidates[i] = &batchwatcher.Output{
			Outpoint: wire.OutPoint{Hash: hash, Index: 0},
			TxOut:    wire.NewTxOut(value, pkScript),
			TreeNode: &treepkg.Node{
				CoSigners: []*btcec.PublicKey{internalKey},
			},
		}
	}

	sweepTx, err := buildSignedSweepTx(
		candidates, keychain.KeyDescriptor{PubKey: sweepPubKey},
		sweepDelay, []byte{0x51}, btcutil.Amount(1), signer,
	)
	require.NoError(t, err)
	require.Len(t, sweepTx.TxIn, numInputs)
	for i := range sweepTx.TxIn {
		require.NotEmpty(t, sweepTx.TxIn[i].Witness,
			"input %d should have witness attached on the final "+
				"tx — witnesses are deferred until after all "+
				"inputs are signed, then applied in one pass",
			i)
	}
}

// TestBuildSignedSweepTxBatchRoot verifies that sweeping a real batch root
// output succeeds when the internal key is a MuSig2 aggregate over the
// operator and client signing keys.
func TestBuildSignedSweepTxBatchRoot(t *testing.T) {
	t.Parallel()

	operatorKey, _ := testutils.CreateKey(10)
	sweepPubKey, signer := testutils.CreateKey(11)
	client1Owner, _ := testutils.CreateKey(12)
	client2Owner, _ := testutils.CreateKey(14)

	sweepDelay := uint32(10)

	vtxo1, err := treepkg.NewVTXODescriptor(
		btcutil.Amount(120_000), client1Owner, operatorKey,
		sweepDelay,
	)
	require.NoError(t, err)

	vtxo2, err := treepkg.NewVTXODescriptor(
		btcutil.Amount(80_000), client2Owner, operatorKey,
		sweepDelay,
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
