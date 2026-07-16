package virtualchannel

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

func TestCosignBackingTxCompletesWitness(t *testing.T) {
	t.Parallel()

	backingTx, cosignInput, clientSig, operatorKey, operatorSigner :=
		testCosignBackingTx(t)

	signedTx, err := CosignBackingTx(
		operatorSigner, operatorKey, backingTx,
		[]CosignInput{{
			BackingVTXO:     cosignInput.BackingVTXO,
			PkScript:        cosignInput.PkScript,
			PolicyTemplate:  cosignInput.PolicyTemplate,
			ClientSignature: clientSig,
		}},
	)
	require.NoError(t, err)
	require.Len(t, signedTx.TxIn[0].Witness, 4)

	prevOut := &wire.TxOut{
		Value:    int64(cosignInput.Amount),
		PkScript: cosignInput.PkScript,
	}
	prevFetcher := txscript.NewCannedPrevOutputFetcher(
		prevOut.PkScript, prevOut.Value,
	)
	sigHashes := txscript.NewTxSigHashes(signedTx, prevFetcher)
	engine, err := txscript.NewEngine(
		prevOut.PkScript, signedTx, 0, txscript.StandardVerifyFlags,
		nil, sigHashes, prevOut.Value, prevFetcher,
	)
	require.NoError(t, err)
	require.NoError(t, engine.Execute())
}

func TestCosignBackingTxRejectsInvalidClientSignature(t *testing.T) {
	t.Parallel()

	backingTx, cosignInput, clientSig, operatorKey, operatorSigner :=
		testCosignBackingTx(t)
	clientSig[0] ^= 1

	_, err := CosignBackingTx(
		operatorSigner, operatorKey, backingTx,
		[]CosignInput{{
			BackingVTXO:     cosignInput.BackingVTXO,
			PkScript:        cosignInput.PkScript,
			PolicyTemplate:  cosignInput.PolicyTemplate,
			ClientSignature: clientSig,
		}},
	)
	require.ErrorContains(t, err, "verify backing input")
}

func testCosignBackingTx(t *testing.T) (*wire.MsgTx, CosignInput, []byte,
	keychain.KeyDescriptor, input.Signer) {

	t.Helper()

	ownerPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	const exitDelay = 144
	policyTemplate, pkScript, err := arkscript.EncodeStandardVTXOArtifacts(
		ownerPriv.PubKey(), operatorPriv.PubKey(), exitDelay,
	)
	require.NoError(t, err)

	backing := BackingVTXO{
		OutPoint: wire.OutPoint{
			Hash:  chainhash.HashH([]byte("virtual-channel-vtxo")),
			Index: 0,
		},
		Amount: btcutil.Amount(100_000),
	}
	backingTx, _, err := BuildBackingTx([]BackingVTXO{backing}, &wire.TxOut{
		Value:    99_000,
		PkScript: []byte{txscript.OP_TRUE},
	})
	require.NoError(t, err)

	prevOut := &wire.TxOut{
		Value:    int64(backing.Amount),
		PkScript: pkScript,
	}
	prevFetcher := txscript.NewCannedPrevOutputFetcher(
		prevOut.PkScript, prevOut.Value,
	)
	sigHashes := txscript.NewTxSigHashes(backingTx, prevFetcher)
	template, params, err := standardVTXOPolicy(policyTemplate, pkScript)
	require.NoError(t, err)
	spendPath, err := collabSpendPath(template, params, pkScript)
	require.NoError(t, err)

	ownerKey := keychain.KeyDescriptor{PubKey: ownerPriv.PubKey()}
	clientSigner := input.NewMockSigner(
		[]*btcec.PrivateKey{ownerPriv}, &chaincfg.RegressionNetParams,
	)
	signDesc := spendPath.SpendInfo.BuildSignDescriptor(
		ownerKey, prevOut, sigHashes, prevFetcher, 0,
	)
	clientSig, err := clientSigner.SignOutputRaw(backingTx, signDesc)
	require.NoError(t, err)

	operatorKey := keychain.KeyDescriptor{PubKey: operatorPriv.PubKey()}
	operatorSigner := input.NewMockSigner(
		[]*btcec.PrivateKey{operatorPriv},
		&chaincfg.RegressionNetParams,
	)

	return backingTx, CosignInput{
		BackingVTXO:    backing,
		PkScript:       pkScript,
		PolicyTemplate: policyTemplate,
	}, clientSig.Serialize(), operatorKey, operatorSigner
}
