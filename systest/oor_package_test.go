//go:build systest

package systest

import (
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	clienttx "github.com/lightninglabs/darepo-client/lib/tx"
	oortx "github.com/lightninglabs/darepo-client/lib/tx/oor"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// TestOORPackageOnRealChainE2E is a docker-backed system test that constructs
// an OOR transfer on a real regtest chain with fee sponsorship:
//
//  1. build and mine a fee-paying checkpoint tx (sponsor input),
//  2. build a fee-less Ark tx (operator script-spend of checkpoint),
//  3. submitpackage {ark, cpfp-child} and mine a second block.
//
// This mirrors the realistic production flow: package relay does not support
// multi-generation packages, so we confirm the checkpoint first, then CPFP the
// Ark tx using the anchor output in the next block.
func TestOORPackageOnRealChainE2E(t *testing.T) {
	ParallelN(t)

	h := NewE2EHarness(t)
	h.Start()

	ctx := t.Context()

	senderLND := h.PrimaryLND()
	operatorLND := h.ServerLND()

	senderKeyDesc, err := senderLND.WalletKit.DeriveNextKey(
		ctx, int32(987_000),
	)
	require.NoError(t, err)

	operatorKeyDesc := h.OperatorPubKey()
	require.NotNil(t, operatorKeyDesc)

	senderSigner := NewLNDRPCSigner(senderLND.Signer, 30*time.Second)
	operatorSigner := NewLNDRPCSigner(operatorLND.Signer, 30*time.Second)

	ownerLeafScript, err := oorOwnerLeafOperatorCheckSig(
		operatorKeyDesc.PubKey,
	)
	require.NoError(t, err)

	policy := arkscript.CheckpointPolicy{
		OperatorKey: operatorKeyDesc.PubKey,
		CSVDelay:    oorExitDelay,
	}

	inputValue := btcutil.Amount(50_000)
	senderKey := keychain.KeyDescriptor{
		KeyLocator: senderKeyDesc.KeyLocator,
		PubKey:     senderKeyDesc.PubKey,
	}
	operatorKey := keychain.KeyDescriptor{
		KeyLocator: operatorKeyDesc.KeyLocator,
		PubKey:     operatorKeyDesc.PubKey,
	}
	minted := oorMintRealVTXO(
		t, h, operatorSigner, operatorKey, senderKey, oorExitDelay,
		inputValue,
	)

	rpc, err := h.BitcoinRPCClient()
	require.NoError(t, err)
	defer rpc.Shutdown()

	fundingOutpoint := minted.Outpoint
	fundingPrevOut := minted.PrevOut
	vtxoTapscript := minted.VTXO.TapScript

	recipientKeyDesc, err := senderLND.WalletKit.DeriveNextKey(
		ctx, int32(987_002),
	)
	require.NoError(t, err)

	recipientTapKey, err := arkscript.VTXOTapKey(
		recipientKeyDesc.PubKey, operatorKeyDesc.PubKey, oorExitDelay,
	)
	require.NoError(t, err)

	recipientPkScript, err := txscript.PayToTaprootScript(recipientTapKey)
	require.NoError(t, err)

	recipients := []oortx.RecipientOutput{
		{
			PkScript: recipientPkScript,
			Value:    inputValue,
		},
	}

	checkpointTapscript, err := arkscript.CheckpointTapScript(
		policy, ownerLeafScript,
	)
	require.NoError(t, err)

	checkpointInternalKey := &arkscript.ARKNUMSKey

	// 1) Build a checkpoint tx and sign the collaborative leaf
	// with both sender and operator keys.
	checkpointPkScript, err := arkscript.CheckpointPkScript(
		policy, ownerLeafScript,
	)
	require.NoError(t, err)

	sponsorKeyDesc, err := operatorLND.WalletKit.DeriveNextKey(
		ctx, int32(987_010),
	)
	require.NoError(t, err)

	sponsorAddr, err := btcutil.NewAddressWitnessPubKeyHash(
		btcutil.Hash160(sponsorKeyDesc.PubKey.SerializeCompressed()),
		oorChainParams,
	)
	require.NoError(t, err)

	sponsorValue := btcutil.Amount(100_000)
	sponsorTxid := h.Harness.Faucet(sponsorAddr.String(), sponsorValue)
	h.Harness.Generate(oorConfirmDepth)

	sponsorHash, err := chainhash.NewHashFromStr(sponsorTxid)
	require.NoError(t, err)

	sponsorTx, err := rpc.GetRawTransaction(sponsorHash)
	require.NoError(t, err)

	sponsorPkScript, err := txscript.PayToAddrScript(sponsorAddr)
	require.NoError(t, err)

	sponsorOutpoint, sponsorPrevOut, err := oorFindOutpoint(
		sponsorTx.MsgTx(), *sponsorHash, sponsorPkScript,
	)
	require.NoError(t, err)

	cpfpKeyDesc, err := operatorLND.WalletKit.DeriveNextKey(
		ctx, int32(987_011),
	)
	require.NoError(t, err)

	cpfpAddr, err := btcutil.NewAddressWitnessPubKeyHash(
		btcutil.Hash160(cpfpKeyDesc.PubKey.SerializeCompressed()),
		oorChainParams,
	)
	require.NoError(t, err)

	cpfpValue := btcutil.Amount(150_000)
	cpfpFundingTxid := h.Harness.Faucet(cpfpAddr.String(), cpfpValue)
	h.Harness.Generate(oorConfirmDepth)

	cpfpHash, err := chainhash.NewHashFromStr(cpfpFundingTxid)
	require.NoError(t, err)

	cpfpFundTx, err := rpc.GetRawTransaction(cpfpHash)
	require.NoError(t, err)

	cpfpPkScript, err := txscript.PayToAddrScript(cpfpAddr)
	require.NoError(t, err)

	cpfpOutpoint, cpfpPrevOut, err := oorFindOutpoint(
		cpfpFundTx.MsgTx(), *cpfpHash, cpfpPkScript,
	)
	require.NoError(t, err)

	checkpointTx := wire.NewMsgTx(3)
	checkpointTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: fundingOutpoint,
		Sequence:         wire.MaxTxInSequenceNum,
	})
	checkpointTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: sponsorOutpoint,
		Sequence:         wire.MaxTxInSequenceNum,
	})
	checkpointTx.AddTxOut(&wire.TxOut{
		Value:    int64(inputValue),
		PkScript: checkpointPkScript,
	})
	sponsorChange := sponsorPrevOut.Value - oorCheckpointFeeSat
	require.Greater(t, sponsorChange, int64(0),
		"sponsor utxo too small for checkpoint fee",
	)
	checkpointTx.AddTxOut(&wire.TxOut{
		Value:    sponsorChange,
		PkScript: sponsorPrevOut.PkScript,
	})

	checkpointPrevFetcher := &mapPrevOutputFetcher{
		prev: map[wire.OutPoint]*wire.TxOut{
			fundingOutpoint: fundingPrevOut,
			sponsorOutpoint: sponsorPrevOut,
		},
	}
	checkpointSigHashes := txscript.NewTxSigHashes(
		checkpointTx, checkpointPrevFetcher,
	)

	clientSignDesc, spendInfo, err := clienttx.NewVTXOCollabSignDescriptor(
		&clienttx.VTXOSpendContext{
			Outpoint:  fundingOutpoint,
			Output:    fundingPrevOut,
			TapScript: vtxoTapscript,
		},
		senderKey,
		0,
		checkpointSigHashes,
		checkpointPrevFetcher,
	)
	require.NoError(t, err)

	operatorSignDesc, _, err := clienttx.NewVTXOCollabSignDescriptor(
		&clienttx.VTXOSpendContext{
			Outpoint:  fundingOutpoint,
			Output:    fundingPrevOut,
			TapScript: vtxoTapscript,
		},
		operatorKey,
		0,
		checkpointSigHashes,
		checkpointPrevFetcher,
	)
	require.NoError(t, err)

	clientSig, err := senderSigner.SignOutputRaw(
		checkpointTx, clientSignDesc,
	)
	require.NoError(t, err)

	operatorSig, err := operatorSigner.SignOutputRaw(
		checkpointTx, operatorSignDesc,
	)
	require.NoError(t, err)

	// The witness stack must provide the cosigner signature first
	// so the owner signature is on top for the initial
	// OP_CHECKSIGVERIFY.
	checkpointTx.TxIn[0].Witness = wire.TxWitness{
		operatorSig.Serialize(),
		clientSig.Serialize(),
		spendInfo.WitnessScript,
		spendInfo.ControlBlock,
	}

	sponsorScript, err := operatorSigner.ComputeInputScript(
		checkpointTx, &input.SignDescriptor{
			KeyDesc: keychain.KeyDescriptor{
				KeyLocator: sponsorKeyDesc.KeyLocator,
				PubKey:     sponsorKeyDesc.PubKey,
			},
			Output:     sponsorPrevOut,
			HashType:   txscript.SigHashAll,
			SignMethod: input.WitnessV0SignMethod,
			InputIndex: 1,
		},
	)
	require.NoError(t, err)

	checkpointTx.TxIn[1].Witness = sponsorScript.Witness
	checkpointTx.TxIn[1].SignatureScript = sponsorScript.SigScript

	// We'll derive the checkpoint txid now so we can build the rest of the
	// virtual chain (ark + cpfp) before broadcasting anything.
	checkpointTxid := checkpointTx.TxHash()

	// 2) Build the Ark tx PSBT (fee-less), spending the
	// checkpoint output. We reuse the checkpoint builder to compute
	// the canonical tap tree encoding.
	checkpointInput := oortx.CheckpointInput{
		SpentVTXO: oortx.SpentVTXORef{
			Outpoint: fundingOutpoint,
			Output:   fundingPrevOut,
		},
		OwnerLeafScript: ownerLeafScript,
	}
	checkpointRes, err := oortx.BuildCheckpointPSBT(policy, checkpointInput)
	require.NoError(t, err)

	checkpointOutputs := []oortx.CheckpointOutput{
		{
			Txid:           checkpointTxid,
			Output:         checkpointTx.TxOut[0],
			TapTreeEncoded: checkpointRes.TapTreeEncoded,
		},
	}
	arkPkt, err := oortx.BuildArkPSBT(checkpointOutputs, recipients)
	require.NoError(t, err)

	// 3) Sign the Ark tx by spending the checkpoint output using
	// the owner leaf script, then submitpackage {ark, cpfp-child}.
	arkTx := arkPkt.UnsignedTx.Copy()

	cpOut := checkpointTx.TxOut[0]
	arkPrevFetcher := txscript.NewCannedPrevOutputFetcher(
		cpOut.PkScript, cpOut.Value,
	)
	arkSigHashes := txscript.NewTxSigHashes(arkTx, arkPrevFetcher)

	cpLeaf := txscript.NewBaseTapLeaf(ownerLeafScript)
	tree := txscript.AssembleTaprootScriptTree(
		checkpointTapscript.Leaves...,
	)
	proofIdx, ok := tree.LeafProofIndex[cpLeaf.TapHash()]
	require.True(t, ok)
	proof := tree.LeafMerkleProofs[proofIdx]
	ctrl := proof.ToControlBlock(checkpointInternalKey)
	ctrlBytes, err := ctrl.ToBytes()
	require.NoError(t, err)

	arkSig, err := operatorSigner.SignOutputRaw(arkTx,
		&input.SignDescriptor{
			KeyDesc:           operatorKey,
			WitnessScript:     ownerLeafScript,
			SignMethod:        input.TaprootScriptSpendSignMethod,
			Output:            cpOut,
			HashType:          txscript.SigHashDefault,
			SigHashes:         arkSigHashes,
			PrevOutputFetcher: arkPrevFetcher,
			InputIndex:        0,
		},
	)
	require.NoError(t, err)

	arkTx.TxIn[0].Witness = wire.TxWitness{
		arkSig.Serialize(),
		ownerLeafScript,
		ctrlBytes,
	}

	bitcoind, err := h.BitcoindClient()
	require.NoError(t, err)

	anchorIndex := uint32(len(arkTx.TxOut) - 1)
	anchorOut := wire.OutPoint{
		Hash:  arkTx.TxHash(),
		Index: anchorIndex,
	}

	cpfpChange := cpfpPrevOut.Value - oorCPFPPackageFeeSat
	require.Greater(t, cpfpChange, int64(0),
		"cpfp utxo too small for package fee",
	)

	child := wire.NewMsgTx(3)
	child.AddTxIn(&wire.TxIn{
		PreviousOutPoint: anchorOut,
		Sequence:         wire.MaxTxInSequenceNum,
	})
	child.AddTxIn(&wire.TxIn{
		PreviousOutPoint: cpfpOutpoint,
		Sequence:         wire.MaxTxInSequenceNum,
	})
	child.AddTxOut(&wire.TxOut{
		Value:    cpfpChange,
		PkScript: cpfpPrevOut.PkScript,
	})

	childScript, err := operatorSigner.ComputeInputScript(
		child, &input.SignDescriptor{
			KeyDesc: keychain.KeyDescriptor{
				KeyLocator: cpfpKeyDesc.KeyLocator,
				PubKey:     cpfpKeyDesc.PubKey,
			},
			Output:     cpfpPrevOut,
			HashType:   txscript.SigHashAll,
			SignMethod: input.WitnessV0SignMethod,
			InputIndex: 1,
		},
	)
	require.NoError(t, err)

	child.TxIn[1].Witness = childScript.Witness
	child.TxIn[1].SignatureScript = childScript.SigScript

	// Now that the full virtual chain is constructed and signed,
	// broadcast and mine gradually.
	checkpointTxidPtr, err := rpc.SendRawTransaction(checkpointTx, false)
	require.NoError(t, err, "broadcast checkpoint tx")
	require.Equal(t, checkpointTx.TxHash(), *checkpointTxidPtr)

	minedCheckpoint := h.Harness.GenerateAndWait(1)
	require.NotEmpty(t, minedCheckpoint)

	foundCheckpoint := false
	for _, txid := range minedCheckpoint[len(minedCheckpoint)-1].TxIDs {
		if txid == checkpointTxid.String() {
			foundCheckpoint = true
			break
		}
	}
	require.True(t, foundCheckpoint, "checkpoint not mined")

	pkgResult, err := bitcoind.SubmitPackage(
		[]*wire.MsgTx{arkTx}, child, nil,
	)
	require.NoError(t, err)
	if pkgResult.PackageMsg != "success" {
		for wtxid, txRes := range pkgResult.TxResults {
			if txRes.Error == nil || *txRes.Error == "" {
				continue
			}

			t.Logf("submitpackage tx wtxid=%s txid=%s err=%s",
				wtxid, txRes.TxID.String(), *txRes.Error)
		}

		t.Fatalf("submitpackage failed: %s", pkgResult.PackageMsg)
	}

	blocks := h.Harness.GenerateAndWait(1)
	require.NotEmpty(t, blocks)

	last := blocks[len(blocks)-1]
	want := map[string]bool{
		arkTx.TxHash().String(): false,
		child.TxHash().String(): false,
	}
	for _, txid := range last.TxIDs {
		if _, ok := want[txid]; ok {
			want[txid] = true
		}
	}

	for txid, ok := range want {
		require.True(t, ok, "missing mined txid %s", txid)
	}
}
