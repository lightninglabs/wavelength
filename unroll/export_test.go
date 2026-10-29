package unroll

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/lib/recovery"
	"github.com/lightninglabs/wavelength/lib/tree"
	"github.com/stretchr/testify/require"
)

// signedExportMaterial builds two signed round roots feeding a multi-input
// OOR transaction. All signatures commit to real taproot prevouts.
func signedExportMaterial(t *testing.T) *LineageMaterial {
	t.Helper()
	key, _ := btcec.PrivKeyFromBytes([]byte{1})
	script, err := txscript.PayToTaprootScript(
		txscript.ComputeTaprootKeyNoScript(
			key.PubKey(),
		),
	)
	require.NoError(t, err)
	material := &LineageMaterial{CSVDelay: 144}
	prevouts := make(map[wire.OutPoint]*wire.TxOut)
	spend := wire.NewMsgTx(3)
	for i := byte(1); i <= 2; i++ {
		batch := wire.OutPoint{Hash: chainhash.Hash{i}}
		output := &wire.TxOut{Value: 50_000, PkScript: script}
		root := &tree.Node{
			Input: batch,
			Outputs: []*wire.TxOut{
				output, arkscript.AnchorOutput(),
			},
		}
		tx, err := root.ToTx()
		require.NoError(t, err)
		fetcher := txscript.NewCannedPrevOutputFetcher(
			script, output.Value,
		)
		sig, err := txscript.RawTxInTaprootSignature(
			tx, txscript.NewTxSigHashes(tx, fetcher), 0,
			output.Value, script, nil, txscript.SigHashDefault, key,
		)
		require.NoError(t, err)
		root.Signature, err = schnorr.ParseSignature(sig)
		require.NoError(t, err)
		material.TreePaths = append(material.TreePaths, &tree.Tree{
			BatchOutpoint: batch, BatchOutput: output, Root: root,
		})
		outpoint := wire.OutPoint{Hash: tx.TxHash()}
		prevouts[outpoint] = output
		spend.AddTxIn(wire.NewTxIn(&outpoint, nil, nil))
	}
	spend.AddTxOut(&wire.TxOut{Value: 100_000, PkScript: script})
	spend.AddTxOut(arkscript.AnchorOutput())
	fetcher := txscript.NewMultiPrevOutFetcher(prevouts)
	hashes := txscript.NewTxSigHashes(spend, fetcher)
	for i := range spend.TxIn {
		sig, err := txscript.RawTxInTaprootSignature(
			spend, hashes, i, 50_000, script, nil,
			txscript.SigHashDefault, key,
		)
		require.NoError(t, err)
		spend.TxIn[i].Witness = wire.TxWitness{sig}
	}
	material.ExtraNodes = []*recovery.Node{{
		Kind: recovery.NodeKindArk, Tx: spend,
	}}
	material.TargetOutpoint = wire.OutPoint{Hash: spend.TxHash()}

	return material
}

// TestBuildSignedAncestry verifies fan-in dependencies and all input witnesses
// survive the existing proof codec, with no wallet or chain service involved.
func TestBuildSignedAncestry(t *testing.T) {
	t.Parallel()
	material := signedExportMaterial(t)
	exported, err := BuildSignedAncestry(material)
	require.NoError(t, err)
	require.Len(t, exported.ExternalInputs, 2)
	require.Len(t, exported.Proof.Layers(), 2)
	parents, err := exported.Proof.ParentTxids(material.TargetOutpoint.Hash)
	require.NoError(t, err)
	require.Len(t, parents, 2)
	raw, err := recovery.EncodeProof(exported.Proof)
	require.NoError(t, err)
	decoded, err := recovery.DecodeProof(raw)
	require.NoError(t, err)
	require.Equal(
		t, exported.Proof.TargetOutpoint(), decoded.TargetOutpoint(),
	)
	for _, layer := range decoded.Layers() {
		for _, txid := range layer {
			before, _ := exported.Proof.Node(txid)
			after, _ := decoded.Node(txid)
			require.Equal(
				t, before.Tx.WitnessHash(),
				after.Tx.WitnessHash(),
			)
		}
	}
}

// TestBuildSignedAncestryRejectsIncomplete rejects graph-valid artifacts that
// would fail broadcast, including invalid signatures on either fan-in input.
func TestBuildSignedAncestryRejectsIncomplete(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		mutate func(*LineageMaterial)
	}{
		{
			name: "unsigned tree",
			mutate: func(m *LineageMaterial) {
				m.TreePaths[0].Root.Signature = nil
			},
		},
		{
			name: "missing commitment output",
			mutate: func(m *LineageMaterial) {
				m.TreePaths[0].BatchOutput = nil
			},
		},
		{
			name: "incorrect commitment value",
			mutate: func(m *LineageMaterial) {
				output := *m.TreePaths[0].BatchOutput
				output.Value++
				m.TreePaths[0].BatchOutput = &output
			},
		},
		{
			name: "wrong external output index",
			mutate: func(m *LineageMaterial) {
				m.TreePaths[0].BatchOutpoint.Index++
			},
		},
		{
			name: "unsigned second input",
			mutate: func(m *LineageMaterial) {
				m.ExtraNodes[0].Tx.TxIn[1].Witness = nil
			},
		},
		{
			name: "invalid second signature",
			mutate: func(m *LineageMaterial) {
				m.ExtraNodes[0].Tx.TxIn[1].Witness[0][0] ^= 1
			},
		},
		{
			name: "conflicting duplicate commitment",
			mutate: func(m *LineageMaterial) {
				path := *m.TreePaths[0]
				output := *path.BatchOutput
				output.Value++
				path.BatchOutput = &output
				m.TreePaths = append(m.TreePaths, &path)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			material := signedExportMaterial(t)
			tc.mutate(material)
			exported, err := BuildSignedAncestry(material)
			require.Error(t, err)
			require.Nil(t, exported)
		})
	}
}

// TestBuildSignedAncestryRejectsOverspend refuses a transaction with valid
// signatures but insufficient input value, which script validation alone
// would accept.
func TestBuildSignedAncestryRejectsOverspend(t *testing.T) {
	t.Parallel()
	material := signedExportMaterial(t)
	path := material.TreePaths[0]
	root := path.Root
	root.Outputs[0] = &wire.TxOut{
		Value: 50_001, PkScript: path.BatchOutput.PkScript,
	}
	tx, err := root.ToTx()
	require.NoError(t, err)
	key, _ := btcec.PrivKeyFromBytes([]byte{1})
	fetcher := txscript.NewCannedPrevOutputFetcher(
		path.BatchOutput.PkScript, path.BatchOutput.Value,
	)
	sig, err := txscript.RawTxInTaprootSignature(
		tx, txscript.NewTxSigHashes(tx, fetcher), 0, 50_000,
		path.BatchOutput.PkScript, nil, txscript.SigHashDefault, key,
	)
	require.NoError(t, err)
	root.Signature, err = schnorr.ParseSignature(sig)
	require.NoError(t, err)
	material.TreePaths = material.TreePaths[:1]
	material.ExtraNodes = nil
	material.TargetOutpoint = wire.OutPoint{Hash: tx.TxHash()}
	_, err = BuildSignedAncestry(material)
	require.ErrorContains(t, err, "spends more than its inputs")
}
