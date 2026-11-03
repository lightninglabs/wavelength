//go:build systest

package systest

import (
	"bytes"
	"strings"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/lib/recovery"
	"github.com/lightninglabs/wavelength/lib/tree"
	"github.com/lightninglabs/wavelength/unroll"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// exportSweepSigner signs timeout leaves using the owner's VTXO key.
// The service's CPFP signer below has a different key and no owner access.
type exportSweepSigner struct {
	input.Signer
	key *btcec.PrivateKey
}

// SignOutputRaw signs the full tapscript spend, including its anchor output.
func (s *exportSweepSigner) SignOutputRaw(tx *wire.MsgTx,
	desc *input.SignDescriptor) (input.Signature, error) {

	digest, err := txscript.CalcTapscriptSignaturehash(
		desc.SigHashes, desc.HashType, tx, desc.InputIndex,
		desc.PrevOutputFetcher,
		txscript.NewBaseTapLeaf(desc.WitnessScript),
	)
	if err != nil {
		return nil, err
	}

	return schnorr.Sign(s.key, digest)
}

// TestSignedAncestryExternalCPFP submits exported zero-fee ancestry to Bitcoin
// Core with a CPFP child signed and funded by an independent service key. The
// executor also completes a presigned timeout sweep after CSV maturity,
// using only its own confirmed change to fund the final CPFP child.
func TestSignedAncestryExternalCPFP(t *testing.T) {
	ParallelN(t)
	h := NewSysTestHarness(t)
	client, err := h.Harness.BitcoinRPCClient()
	require.NoError(t, err)
	t.Cleanup(client.Shutdown)
	owner, _ := btcec.PrivKeyFromBytes([]byte{1})
	service, _ := btcec.PrivKeyFromBytes([]byte{2})
	ownerScript, err := txscript.PayToTaprootScript(
		txscript.ComputeTaprootKeyNoScript(
			owner.PubKey(),
		),
	)
	require.NoError(t, err)
	serviceScript, err := txscript.PayToTaprootScript(
		txscript.ComputeTaprootKeyNoScript(
			service.PubKey(),
		),
	)
	require.NoError(t, err)

	// Fund the commitment and the service separately, confirming each
	// before asking the harness wallet to select another funding input.
	commitment := h.Harness.SignedV3Tx(ownerScript, 50_000)
	_, err = client.SendRawTransaction(commitment, false)
	require.NoError(t, err)
	h.Harness.Generate(1)
	serviceFunding := h.Harness.SignedV3Tx(serviceScript, 50_000)
	_, err = client.SendRawTransaction(serviceFunding, false)
	require.NoError(t, err)
	h.Harness.Generate(1)

	const csvDelay uint32 = 2
	timeout, err := arkscript.VTXOTapScript(
		owner.PubKey(), service.PubKey(), csvDelay,
	)
	require.NoError(t, err)
	targetScript, err := txscript.PayToTaprootScript(
		txscript.ComputeTaprootOutputKey(
			timeout.ControlBlock.InternalKey, timeout.RootHash,
		),
	)
	require.NoError(t, err)

	batch := wire.OutPoint{Hash: commitment.TxHash()}
	root := &tree.Node{
		Input: batch,
		Outputs: []*wire.TxOut{
			{
				Value:    50_000,
				PkScript: targetScript,
			},
			arkscript.AnchorOutput(),
		},
	}
	tx, err := root.ToTx()
	require.NoError(t, err)
	fetcher := txscript.NewCannedPrevOutputFetcher(ownerScript, 50_000)
	sig, err := txscript.RawTxInTaprootSignature(
		tx, txscript.NewTxSigHashes(tx, fetcher), 0, 50_000,
		ownerScript, nil, txscript.SigHashDefault, owner,
	)
	require.NoError(t, err)
	root.Signature, err = schnorr.ParseSignature(sig)
	require.NoError(t, err)
	exported, err := unroll.BuildSignedAncestry(&unroll.LineageMaterial{
		TargetOutpoint: wire.OutPoint{Hash: tx.TxHash()},
		CSVDelay:       csvDelay,
		TreePaths: []*tree.Tree{{
			BatchOutpoint: batch, BatchOutput: commitment.TxOut[0],
			Root: root,
		}},
	})
	require.NoError(t, err)
	raw, err := recovery.EncodeProof(exported.Proof)
	require.NoError(t, err)
	proof, err := recovery.DecodeProof(raw)
	require.NoError(t, err)
	node, err := proof.TargetNode()
	require.NoError(t, err)
	anchor, found, err := node.AnchorOutpoint()
	require.NoError(t, err)
	require.True(t, found)

	// Presign before any ancestry has been submitted, then transfer only
	// the final wire transaction to the service.
	presigned, err := unroll.BuildSignedExitSweep(&vtxo.Descriptor{
		Outpoint: proof.TargetOutpoint(), Amount: 50_000,
		RelativeExpiry: csvDelay, PkScript: targetScript,
		ClientKey:   keychain.KeyDescriptor{PubKey: owner.PubKey()},
		OperatorKey: service.PubKey(),
	}, proof, ownerScript, &exportSweepSigner{key: owner})
	require.NoError(t, err)
	var encoded bytes.Buffer
	require.NoError(t, presigned.Serialize(&encoded))
	var finalSweep wire.MsgTx
	require.NoError(
		t,
		finalSweep.Deserialize(
			bytes.NewReader(
				encoded.Bytes(),
			),
		),
	)

	// From here the broadcaster uses only exported data and the service's
	// own key. The zero-fee parent is relayed together with its child.
	feeInput := wire.OutPoint{Hash: serviceFunding.TxHash()}
	child := wire.NewMsgTx(3)
	child.AddTxIn(wire.NewTxIn(&anchor, nil, nil))
	child.AddTxIn(wire.NewTxIn(&feeInput, nil, nil))
	child.AddTxOut(&wire.TxOut{Value: 48_000, PkScript: serviceScript})
	previous := txscript.NewMultiPrevOutFetcher(
		map[wire.OutPoint]*wire.TxOut{
			anchor:   node.Tx.TxOut[anchor.Index],
			feeInput: serviceFunding.TxOut[0],
		},
	)
	serviceSig, err := txscript.RawTxInTaprootSignature(
		child, txscript.NewTxSigHashes(child, previous), 1, 50_000,
		serviceScript, nil, txscript.SigHashDefault, service,
	)
	require.NoError(t, err)
	child.TxIn[1].Witness = wire.TxWitness{serviceSig}
	result, err := client.SubmitPackage(
		[]*wire.MsgTx{node.Tx, child}, nil, nil,
	)
	require.NoError(t, err)
	require.Equal(t, "success", result.PackageMsg, "%+v", result)
	parentHash := node.Tx.TxHash()
	childHash := child.TxHash()
	mempool, err := client.GetRawMempool()
	require.NoError(t, err)
	require.Contains(t, mempool, &parentHash)
	require.Contains(t, mempool, &childHash)
	h.Harness.Generate(1)
	confirmed, err := client.GetRawTransactionVerbose(&parentHash)
	require.NoError(t, err)
	require.Positive(t, confirmed.Confirmations)
	// The first CPFP child's change is now a confirmed service-owned fee
	// input. Its value, rather than the owner's payout, funds the sweep.
	finalChild := child.Copy()
	finalChild.TxIn[0].PreviousOutPoint = wire.OutPoint{
		Hash: finalSweep.TxHash(), Index: 1,
	}
	finalChild.TxIn[1].PreviousOutPoint = wire.OutPoint{Hash: childHash}
	finalChild.TxIn[1].Witness = nil
	finalChild.TxOut[0].Value = 46_000
	finalAnchor := finalChild.TxIn[0].PreviousOutPoint
	finalFeeInput := finalChild.TxIn[1].PreviousOutPoint
	finalPrevious := txscript.NewMultiPrevOutFetcher(
		map[wire.OutPoint]*wire.TxOut{
			finalAnchor:   finalSweep.TxOut[1],
			finalFeeInput: child.TxOut[0],
		},
	)
	finalSig, err := txscript.RawTxInTaprootSignature(
		finalChild, txscript.NewTxSigHashes(finalChild, finalPrevious),
		1, 48_000, serviceScript, nil, txscript.SigHashDefault, service,
	)
	require.NoError(t, err)
	finalChild.TxIn[1].Witness = wire.TxWitness{finalSig}
	early, err := client.SubmitPackage(
		[]*wire.MsgTx{&finalSweep, finalChild}, nil, nil,
	)
	require.NoError(t, err)
	require.NotEqual(t, "success", early.PackageMsg)
	immature := false
	for _, result := range early.TxResults {
		if result.Error != nil &&
			strings.Contains(*result.Error, "non-BIP68-final") {

			immature = true
		}
	}
	require.True(
		t, immature, "premature sweep must fail CSV maturity: %+v",
		early,
	)
	h.Harness.Generate(int(csvDelay))
	completed, err := client.SubmitPackage(
		[]*wire.MsgTx{&finalSweep, finalChild}, nil, nil,
	)
	require.NoError(t, err)
	require.Equal(t, "success", completed.PackageMsg, "%+v", completed)
	h.Harness.Generate(1)
	finalHash := finalSweep.TxHash()
	exit, err := client.GetRawTransactionVerbose(&finalHash)
	require.NoError(t, err)
	require.Positive(t, exit.Confirmations)
	require.Equal(t, int64(50_000), finalSweep.TxOut[0].Value)
	require.Equal(t, ownerScript, finalSweep.TxOut[0].PkScript)
}
