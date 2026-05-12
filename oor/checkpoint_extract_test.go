package oor

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	checkpointtx "github.com/lightninglabs/darepo-client/lib/tx/checkpoint"
	"github.com/lightninglabs/darepo-client/lib/tx/psbtutil"
	"github.com/stretchr/testify/require"
)

// TestExtractCheckpointTxRoundTripsCollabSpend verifies that the helper
// reassembles a broadcast-ready transaction from a finalized OOR checkpoint
// PSBT that was signed by the standard 2-of-2 collaborative leaf — i.e. with
// only TaprootScriptSpendSig and TaprootLeafScript populated, and no
// FinalScriptWitness assembled. This is the exact shape persisted by the OOR
// finalize path and the case the production fraud responder hits.
func TestExtractCheckpointTxRoundTripsCollabSpend(t *testing.T) {
	t.Parallel()

	pkt, ownerKey, operatorKey := buildSignedCheckpointPSBT(t)

	tx, err := extractCheckpointTx(pkt)
	require.NoError(t, err)
	require.NotNil(t, tx)
	require.Equal(t, pkt.UnsignedTx.TxHash(), tx.TxHash())
	require.Len(t, tx.TxIn[0].Witness, 4)

	// Run the assembled witness through the script engine to confirm it
	// is a valid spend of the input. This catches witness-order bugs that
	// would not be visible from a structural assertion alone.
	prevOut := pkt.Inputs[0].WitnessUtxo
	prevFetcher := txscript.NewCannedPrevOutputFetcher(
		prevOut.PkScript, prevOut.Value,
	)
	sigHashes := txscript.NewTxSigHashes(tx, prevFetcher)
	engine, err := txscript.NewEngine(
		prevOut.PkScript, tx, 0, txscript.StandardVerifyFlags, nil,
		sigHashes, prevOut.Value, prevFetcher,
	)
	require.NoError(t, err)
	require.NoError(t, engine.Execute())

	// Witness items 0 and 1 must be 64-byte (default sighash) schnorr
	// signatures, in [operatorSig, ownerSig] order. Witness item 2 is the
	// leaf script and item 3 is the control block.
	require.Len(t, tx.TxIn[0].Witness[0], schnorr.SignatureSize)
	require.Len(t, tx.TxIn[0].Witness[1], schnorr.SignatureSize)

	leaf := pkt.Inputs[0].TaprootLeafScript[0]
	require.True(t, bytes.Equal(tx.TxIn[0].Witness[2], leaf.Script))
	require.True(t, bytes.Equal(tx.TxIn[0].Witness[3], leaf.ControlBlock))

	// Sanity: confirm the test fixture really did produce both keys.
	require.NotNil(t, ownerKey)
	require.NotNil(t, operatorKey)
}

// TestExtractCheckpointTxUsesFinalScriptWitness verifies that when the PSBT
// already carries an assembled FinalScriptWitness — the shape used by custom
// (e.g. vHTLC) spends — extractCheckpointTx parses that witness directly
// instead of trying to reassemble from partial signatures.
func TestExtractCheckpointTxUsesFinalScriptWitness(t *testing.T) {
	t.Parallel()

	pkt, _, _ := buildSignedCheckpointPSBT(t)

	customWitness := wire.TxWitness{
		[]byte("operator-sig"),
		[]byte("client-sig"),
		[]byte("preimage"),
		[]byte("script"),
		[]byte("control-block"),
	}

	var buf bytes.Buffer
	require.NoError(t, psbt.WriteTxWitness(&buf, customWitness))
	pkt.Inputs[0].FinalScriptWitness = buf.Bytes()

	tx, err := extractCheckpointTx(pkt)
	require.NoError(t, err)
	require.Equal(t, customWitness, tx.TxIn[0].Witness)
}

// TestExtractCheckpointTxRejectsMalformed verifies the helper fails loudly on
// PSBTs that lack the metadata it needs to assemble a witness.
func TestExtractCheckpointTxRejectsMalformed(t *testing.T) {
	t.Parallel()

	t.Run("nil packet", func(t *testing.T) {
		_, err := extractCheckpointTx(nil)
		require.ErrorContains(t, err, "missing unsigned tx")
	})

	t.Run("multiple tap leaves", func(t *testing.T) {
		pkt, _, _ := buildSignedCheckpointPSBT(t)
		extra := *pkt.Inputs[0].TaprootLeafScript[0]
		pkt.Inputs[0].TaprootLeafScript = append(
			pkt.Inputs[0].TaprootLeafScript, &extra,
		)

		_, err := extractCheckpointTx(pkt)
		require.ErrorContains(t, err, "tap leaves")
	})

	t.Run("missing signature", func(t *testing.T) {
		pkt, _, _ := buildSignedCheckpointPSBT(t)
		pkt.Inputs[0].TaprootScriptSpendSig =
			pkt.Inputs[0].TaprootScriptSpendSig[:1]

		_, err := extractCheckpointTx(pkt)
		require.ErrorContains(t, err, "no signature recorded")
	})

	t.Run("missing witness utxo", func(t *testing.T) {
		pkt, _, _ := buildSignedCheckpointPSBT(t)
		pkt.Inputs[0].WitnessUtxo = nil

		_, err := extractCheckpointTx(pkt)
		require.ErrorContains(t, err, "missing WitnessUtxo")
	})

	t.Run("leaf does not bind to prevout pkScript", func(t *testing.T) {
		pkt, _, _ := buildSignedCheckpointPSBT(t)

		// Replace the prevout pkScript with an unrelated P2TR. The
		// signatures on the persisted leaf are still valid in their
		// own right, but no longer commit to this pkScript — exactly
		// the attack shape the binding check guards against.
		bogus := make([]byte, len(pkt.Inputs[0].WitnessUtxo.PkScript))
		copy(bogus, pkt.Inputs[0].WitnessUtxo.PkScript)
		// Flip a byte inside the 32-byte tweaked output key to make
		// the script genuinely unrelated; the leading OP_1/OP_PUSH32
		// stays so the script remains a valid P2TR shape.
		bogus[3] ^= 0xff
		pkt.Inputs[0].WitnessUtxo.PkScript = bogus

		_, err := extractCheckpointTx(pkt)
		require.ErrorContains(t, err, "does not bind to prevout")
	})
}

// buildSignedCheckpointPSBT produces a finalized OOR checkpoint PSBT with
// both owner and operator signatures attached via TaprootScriptSpendSig but
// without a serialized FinalScriptWitness — mirroring exactly what the
// server's OOR finalize path persists for a standard collaborative leaf.
func buildSignedCheckpointPSBT(t *testing.T) (*psbt.Packet, *btcec.PrivateKey,
	*btcec.PrivateKey) {

	t.Helper()

	ownerPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	policy := arkscript.CheckpointPolicy{
		OperatorKey: operatorPriv.PubKey(),
		CSVDelay:    10,
	}

	ownerLeaf, err := (&arkscript.Multisig{
		Keys: []*btcec.PublicKey{
			ownerPriv.PubKey(), operatorPriv.PubKey(),
		},
	}).Script()
	require.NoError(t, err)

	inputOutpoint := wire.OutPoint{
		Hash: chainhash.Hash{
			0xa1,
			0xa2,
			0xa3,
		},
		Index: 0,
	}

	// Build the parent VTXO output the checkpoint will spend. The exact
	// pkScript does not matter for this helper — what matters is that the
	// signed sighash references a coherent prevout.
	parentScript := mustParentTaprootScript(
		t, ownerPriv.PubKey(), operatorPriv.PubKey(),
	)
	parentOutput := &wire.TxOut{
		Value:    25_000,
		PkScript: parentScript,
	}

	artifact, err := checkpointtx.BuildPSBT(policy, checkpointtx.Input{
		SpentVTXO: checkpointtx.SpentVTXORef{
			Outpoint: inputOutpoint,
			Output:   parentOutput,
		},
		OwnerLeafScript: ownerLeaf,
	})
	require.NoError(t, err)

	pkt := artifact.PSBT
	in := &pkt.Inputs[0]

	// Attach the canonical owner-leaf tapscript to the PSBT input so the
	// extractor finds it during witness assembly. The control block must
	// commit the owner leaf to the parent VTXO's tap tree (not the
	// checkpoint output's tap tree), since this leaf is spending the
	// parent.
	leafTap := txscript.NewBaseTapLeaf(ownerLeaf)
	parentTree := txscript.AssembleTaprootScriptTree(leafTap)
	parentCtrl := parentTree.LeafMerkleProofs[0].ToControlBlock(
		&arkscript.ARKNUMSKey,
	)
	parentCtrlBytes, err := parentCtrl.ToBytes()
	require.NoError(t, err)
	require.NoError(
		t,
		psbtutil.AddTapLeafScript(
			in, &arkscript.SpendInfo{
				WitnessScript: ownerLeaf,
				ControlBlock:  parentCtrlBytes,
			},
		),
	)

	// Sign the owner and operator signatures on the collaborative leaf.
	sigHashes := txscript.NewTxSigHashes(
		pkt.UnsignedTx, txscript.NewCannedPrevOutputFetcher(
			parentOutput.PkScript, parentOutput.Value,
		),
	)
	sigHash, err := txscript.CalcTapscriptSignaturehash(
		sigHashes, txscript.SigHashDefault, pkt.UnsignedTx, 0,
		txscript.NewCannedPrevOutputFetcher(
			parentOutput.PkScript, parentOutput.Value,
		),
		leafTap,
	)
	require.NoError(t, err)

	ownerSig, err := schnorr.Sign(ownerPriv, sigHash)
	require.NoError(t, err)
	operatorSig, err := schnorr.Sign(operatorPriv, sigHash)
	require.NoError(t, err)

	require.NoError(
		t,
		psbtutil.AddTaprootScriptSpendSig(
			in, ownerPriv.PubKey(), ownerLeaf, ownerSig.Serialize(),
			txscript.SigHashDefault,
		),
	)
	require.NoError(
		t,
		psbtutil.AddTaprootScriptSpendSig(
			in, operatorPriv.PubKey(), ownerLeaf,
			operatorSig.Serialize(), txscript.SigHashDefault,
		),
	)

	// The finalize path explicitly does NOT set FinalScriptWitness for
	// standard collaborative leaves — the persisted PSBT only carries
	// partial signatures and the leaf metadata.
	require.Empty(t, in.FinalScriptWitness)

	return pkt, ownerPriv, operatorPriv
}

// mustParentTaprootScript builds a P2TR pkScript whose tap tree commits the
// owner-leaf script for the checkpoint input, so the BIP-341 sighash signs
// against a script that actually binds to the prevout.
func mustParentTaprootScript(t *testing.T,
	ownerKey, operatorKey *btcec.PublicKey) []byte {

	t.Helper()

	ownerLeaf, err := (&arkscript.Multisig{
		Keys: []*btcec.PublicKey{
			ownerKey,
			operatorKey,
		},
	}).Script()
	require.NoError(t, err)

	leaf := txscript.NewBaseTapLeaf(ownerLeaf)
	tree := txscript.AssembleTaprootScriptTree(leaf)

	rootHash := tree.RootNode.TapHash()
	tapKey := txscript.ComputeTaprootOutputKey(
		&arkscript.ARKNUMSKey, rootHash[:],
	)
	pkScript, err := txscript.PayToTaprootScript(tapKey)
	require.NoError(t, err)

	return pkScript
}
