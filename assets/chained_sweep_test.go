package assets_test

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/assets"
	"github.com/lightninglabs/darepo-client/harness"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightninglabs/taproot-assets/taprpc"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/require"
)

// TestChainedAssetSweepsWithPackageRelay tests creating two chained asset
// sweeps without broadcasting, then building and broadcasting both packages
// together, updating proofs with block metadata, sweeping back to wallet,
// and verifying balance updates.
func TestChainedAssetSweepsWithPackageRelay(t *testing.T) {
	// Setup harness with tapd enabled.
	opts := harness.DefaultOptions()
	opts.StartTapd = true
	opts.GroupName = "chained-asset-sweeps"

	h := harness.NewHarness(t, &opts)
	h.Start()
	t.Cleanup(h.Stop)

	// Create boarding fixture with funded Alice.
	const scriptOnly = false
	const csvDelay = uint32(2)
	f := newBoardingFixtureWithAliceBoardingFunded(h, scriptOnly, csvDelay)

	ctx, cancel := context.WithTimeout(t.Context(), defaultTimeout)
	defer cancel()

	// ====================================================================
	// First Sweep: Sweep boarded asset to operator-controlled output
	// with zero-fee anchor (no broadcast).
	// ====================================================================

	builder1 := assets.NewAssetTxBuilder(f.assetID, f.chainParams)

	// Add boarded asset as input with MuSig2 anchor key.
	inputMuSig := &assets.MuSig2Spec{
		Participants: []assets.MuSig2Participant{
			{
				Role: assets.SignerRole("user"),
				PubKey: f.userKey.PubKey().
					SerializeCompressed(),
			},
			{
				Role: assets.SignerRole("operator"),
				PubKey: f.operatorKey.PubKey().
					SerializeCompressed(),
			},
		},
		SortKeys: true,
		Tweaks: assets.MuSig2Tweaks{
			TaprootBIP0086Tweak: true,
		},
	}

	err := builder1.AddAssetInput(assets.InputConfig{
		ProofFile: f.boardingProof.RawProofFile,
		AnchorKey: assets.AnchorKeySpec{
			Mode:   assets.AnchorKeyModeMuSig2,
			MuSig2: inputMuSig,
		},
		Closures: []assets.ScriptClosure{
			csvClosureScript(f.userKey.PubKey(), csvDelay),
		},
	})
	require.NoError(t, err)

	// Generate keys for first sweep output.
	destKey1, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	// Add output (operator-controlled) with a CSV closure committed in the
	// anchor tapscript tree. We still spend via key path, but the closure
	// gives us a sibling branch like real-world ARK trees.
	err = builder1.AddAssetOutput(assets.OutputConfig{
		Amount: f.asset.Amount,
		AnchorKey: assets.AnchorKeySpec{
			Mode: assets.AnchorKeyModeStatic,
			Key:  schnorr.SerializePubKey(destKey1.PubKey()),
		},
		Script: assets.OpTrueUniqueScript(destKey1.PubKey()),
		Closures: []assets.ScriptClosure{
			csvClosureScript(destKey1.PubKey(), csvDelay),
		},
	})
	require.NoError(t, err)

	// Add zero-fee anchor.
	anchorSpec1 := assets.NewEphemeralBTCAnchorSpec()
	anchorSpec1.Description = "anchor-sweep-1"
	err = builder1.AddBTCAnchor(anchorSpec1)
	require.NoError(t, err)

	// Compile the virtual packet.
	_, err = builder1.Compile(ctx)
	require.NoError(t, err)

	// Commit with skip wallet funding.
	err = builder1.Commit(
		ctx, f.operatorClient.AssetWalletClient,
		assets.CommitOptions{SkipWalletFunding: true},
	)
	require.NoError(t, err)

	// Get digest and taproot root for MuSig2 signing.
	digest1, err := builder1.GetKeySpendSigHash(0)
	require.NoError(t, err)

	_, _, taprootRoot1, err := builder1.GetTaprootRoots(0, "")
	require.NoError(t, err)

	// Set up the taproot tweak for MuSig2 signing.
	allPubKeys := []*btcec.PublicKey{
		f.userKey.PubKey(), f.operatorKey.PubKey(),
	}
	tweaks := &input.MuSig2Tweaks{
		TaprootBIP0086Tweak: false,
		TaprootTweak:        taprootRoot1,
	}

	// Create signers and sessions for both parties.
	userSigner := assets.NewLocalMuSig2Signer(f.userKey)
	userSession, err := userSigner.MuSig2CreateSession(
		input.MuSig2Version100RC2, keychain.KeyLocator{},
		allPubKeys, tweaks, nil, nil,
	)
	require.NoError(t, err)

	operatorSigner := assets.NewLocalMuSig2Signer(f.operatorKey)
	operatorSession, err := operatorSigner.MuSig2CreateSession(
		input.MuSig2Version100RC2, keychain.KeyLocator{},
		allPubKeys, tweaks, nil, nil,
	)
	require.NoError(t, err)

	// Exchange nonces.
	_, err = userSigner.MuSig2RegisterNonces(
		userSession.SessionID,
		[][musig2.PubNonceSize]byte{operatorSession.PublicNonce},
	)
	require.NoError(t, err)

	_, err = operatorSigner.MuSig2RegisterNonces(
		operatorSession.SessionID,
		[][musig2.PubNonceSize]byte{userSession.PublicNonce},
	)
	require.NoError(t, err)

	// Both parties create partial signatures.
	_, err = userSigner.MuSig2Sign(userSession.SessionID, digest1, false)
	require.NoError(t, err)

	operatorPartial, err := operatorSigner.MuSig2Sign(
		operatorSession.SessionID, digest1, false,
	)
	require.NoError(t, err)

	// Combine signatures.
	finalSig1, haveAll, err := userSigner.MuSig2CombineSig(
		userSession.SessionID,
		[]*musig2.PartialSignature{operatorPartial},
	)
	require.NoError(t, err)
	require.True(t, haveAll)

	// Apply signature to builder.
	err = builder1.ApplyKeySpendSignature(0, finalSig1.Serialize())
	require.NoError(t, err)

	// Finalize anchor.
	finalPsbt1, err := builder1.FinalizeAnchor(ctx, h.LND.WalletKit)
	require.NoError(t, err)

	require.NoError(t, psbt.MaybeFinalizeAll(finalPsbt1))

	anchorTx1, err := psbt.Extract(finalPsbt1)
	require.NoError(t, err)
	t.Logf("First sweep anchor txid: %s", anchorTx1.TxHash())

	// Debug: check which output has which script type.
	for i, out := range anchorTx1.TxOut {
		t.Logf("Output %d: Value=%d, ScriptLen=%d, Script[0:8]=%x",
			i, out.Value, len(out.PkScript), out.PkScript[:min(8, len(out.PkScript))])
	}

	// Debug: check virtual packet output anchor index.
	activePkts := builder1.ActivePackets()
	if len(activePkts) > 0 {
		vPkt := activePkts[0]
		if len(vPkt.Outputs) > 0 {
			t.Logf("Virtual output 0 AnchorOutputIndex: %d",
				vPkt.Outputs[0].AnchorOutputIndex)
		}
	}

	// ====================================================================
	// Broadcast first sweep and mine it to get confirmed proof.
	// ====================================================================

	btcClient, err := h.BitcoindClient()
	require.NoError(t, err)

	rpcClient, err := h.BitcoinRPCClient()
	require.NoError(t, err)
	t.Cleanup(func() { rpcClient.Shutdown() })

	changeAddr, err := rpcClient.GetNewAddress("")
	require.NoError(t, err)

	walletShim := newWalletKitFundingShim(h.LND.WalletKit)

	// Build CPFP child for first anchor.
	childPsbt1, childTx1, err := builder1.BuildAnchorChild(
		ctx, walletShim,
		assets.AnchorChildOptions{
			ChangeAddress: changeAddr,
			FeeRate:       chainfee.SatPerKWeight(10_000),
		},
	)
	require.NoError(t, err)
	require.NotNil(t, childPsbt1)
	t.Logf("First child txid: %s", childTx1.TxHash())

	// Derive an unconfirmed proof for chaining into the second sweep.
	unconfirmedProof1, err := builder1.Proof(0, nil)
	require.NoError(t, err)
	t.Logf("Generated unconfirmed proof for first sweep")

	// ====================================================================
	// Second Sweep: Use first sweep's confirmed proof as input.
	// ====================================================================

	// Create second builder using the first sweep proof (will be confirmed
	// later).
	builder2 := assets.NewAssetTxBuilder(f.assetID, f.chainParams)

	// Debug: decode proof to see what outpoint it contains.
	proofFile1, err := proof.DecodeFile(unconfirmedProof1)
	require.NoError(t, err)
	lastProof, err := proofFile1.LastProof()
	require.NoError(t, err)
	t.Logf("Proof anchor tx: %s", lastProof.AnchorTx.TxHash())
	t.Logf("Proof PrevOut (where asset came from): %s", lastProof.PrevOut)
	t.Logf("Proof InclusionProof.OutputIndex (where asset is NOW): %d",
		lastProof.InclusionProof.OutputIndex)
	t.Logf("So builder2 should spend: %s:%d",
		lastProof.AnchorTx.TxHash(), lastProof.InclusionProof.OutputIndex)
	if lastProof.InclusionProof.InternalKey != nil {
		t.Logf("Proof InclusionProof.InternalKey: %x",
			lastProof.InclusionProof.InternalKey.SerializeCompressed())
	} else {
		t.Logf("Proof InclusionProof.InternalKey is nil")
	}
	t.Logf("destKey1 PubKey: %x", destKey1.PubKey().SerializeCompressed())

	// Use the unconfirmed proof as input (will be updated after broadcast).
	err = builder2.AddAssetInput(assets.InputConfig{
		ProofFile: unconfirmedProof1,
		AnchorKey: assets.AnchorKeySpec{
			Mode: assets.AnchorKeyModeStatic,
			Key:  schnorr.SerializePubKey(destKey1.PubKey()),
		},
	})
	require.NoError(t, err)

	// Debug: Check what outpoint builder2 will actually spend.
	anchorPsbt2AfterInput := builder2.AnchorPsbt()
	if anchorPsbt2AfterInput != nil && len(anchorPsbt2AfterInput.UnsignedTx.TxIn) > 0 {
		t.Logf("Builder2 input 0 will spend: %s",
			anchorPsbt2AfterInput.UnsignedTx.TxIn[0].PreviousOutPoint)
	}

	// Generate keys for second sweep output.
	destKey2, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	// Add output for second sweep.
	err = builder2.AddAssetOutput(assets.OutputConfig{
		Amount: f.asset.Amount,
		AnchorKey: assets.AnchorKeySpec{
			Mode: assets.AnchorKeyModeStatic,
			Key:  schnorr.SerializePubKey(destKey2.PubKey()),
		},
		Script: assets.OpTrueUniqueScript(destKey2.PubKey()),
	})
	require.NoError(t, err)

	// Add zero-fee anchor for second sweep.
	anchorSpec2 := assets.NewEphemeralBTCAnchorSpec()
	anchorSpec2.Description = "anchor-sweep-2"
	err = builder2.AddBTCAnchor(anchorSpec2)
	require.NoError(t, err)

	// Compile, commit with skip wallet funding (zero-fee).
	_, err = builder2.Compile(ctx)
	require.NoError(t, err)

	err = builder2.Commit(
		ctx, f.operatorClient.AssetWalletClient,
		assets.CommitOptions{SkipWalletFunding: true},
	)
	require.NoError(t, err)

	// OpTrueUniqueScript anchor outputs are spent via key-path spend using
	// the internal key. Since there's a tapscript tree (asset commitment),
	// we need to tweak the private key before signing. The builder only
	// caches script roots when closures are present, so derive the
	// taproot root directly from the proof.
	taprootTweak2, err := assets.GenTaprootRootFromProof(lastProof)
	require.NoError(t, err)

	// Tweak the private key.
	tweakedKey := txscript.TweakTaprootPrivKey(*destKey1, taprootTweak2)

	// Get the sighash.
	digest2, err := builder2.GetKeySpendSigHash(0)
	require.NoError(t, err)

	// Sign with the tweaked private key.
	sig2, err := schnorr.Sign(tweakedKey, digest2[:])
	require.NoError(t, err)

	// Apply the signature.
	err = builder2.ApplyKeySpendSignature(0, sig2.Serialize())
	require.NoError(t, err)

	finalPsbt2, err := builder2.FinalizeAnchor(ctx, h.LND.WalletKit)
	require.NoError(t, err)

	// Finalize the PSBT to compute the witness from the signature.
	require.NoError(t, psbt.MaybeFinalizeAll(finalPsbt2))

	// Debug: check PSBT state before extraction.
	t.Logf("PSBT has %d inputs", len(finalPsbt2.Inputs))
	for i := range finalPsbt2.Inputs {
		t.Logf("  Input %d: hasWitness=%v, witnessLen=%d", i,
			finalPsbt2.UnsignedTx.TxIn[i].Witness != nil,
			len(finalPsbt2.UnsignedTx.TxIn[i].Witness))
	}

	// Skip MaybeFinalizeAll since the witness is already set on the
	// transaction. Extract can work with partially-finalized PSBTs.
	anchorTx2, err := psbt.Extract(finalPsbt2)
	require.NoError(t, err)
	t.Logf("Second sweep anchor txid: %s", anchorTx2.TxHash())

	// Capture transfer data for proof construction after external broadcast.
	transferData2, err := builder2.GetTransferData()
	require.NoError(t, err)

	// ====================================================================
	// Build CPFP child for second anchor (broadcast later).
	// ====================================================================

	// Build CPFP child for second anchor.
	childPsbt2, childTx2, err := builder2.BuildAnchorChild(
		ctx, walletShim,
		assets.AnchorChildOptions{
			ChangeAddress: changeAddr,
			FeeRate:       chainfee.SatPerKWeight(10_000),
		},
	)
	require.NoError(t, err)
	require.NotNil(t, childPsbt2)
	t.Logf("Second child txid: %s", childTx2.TxHash())

	// Broadcast packages in dependency order and update proofs.
	// ====================================================================

	// Submit package for first sweep.
	submitRes1, err := btcClient.SubmitPackage(
		[]*wire.MsgTx{anchorTx1}, childTx1, nil,
	)
	require.NoError(t, err)
	require.NotNil(t, submitRes1)
	for txid, res := range submitRes1.TxResults {
		if res.Error != nil {
			t.Fatalf("tx %s rejected: %s", txid, *res.Error)
		}
	}
	t.Logf("Package 1 submitted successfully")

	_, err = builder1.Publish(
		ctx, f.operatorClient.TapdClient, "sweep-1",
		assets.PublishOptions{},
	)
	require.NoError(t, err)

	// Mine first sweep and confirm proof.
	minedBlocks1 := h.GenerateAndWait(1)
	minedBlock1 := minedBlocks1[0]
	t.Logf("Mined first sweep at height %d", minedBlock1.Header.Height)

	blockHash1, err := chainhash.NewHashFromStr(minedBlock1.Header.Hash)
	require.NoError(t, err)

	rawBlock1, err := rpcClient.GetBlock(blockHash1)
	require.NoError(t, err)

	var txIndex1 int
	anchorTx1Hash := anchorTx1.TxHash()
	for i, tx := range rawBlock1.Transactions {
		txHash := tx.TxHash()
		if txHash.IsEqual(&anchorTx1Hash) {
			txIndex1 = i
			t.Logf("Found anchor1 at index %d", i)
			break
		}
	}

	confirmedProof1, err := builder1.Proof(0, &assets.ProofParams{
		Block:       rawBlock1,
		BlockHeight: uint32(minedBlock1.Header.Height),
		TxIndex:     txIndex1,
	})
	require.NoError(t, err)
	t.Logf("Generated confirmed proof for first sweep")

	_, err = f.operatorClient.TapdClient.ImportProofFile(ctx, confirmedProof1)
	require.NoError(t, err)
	t.Logf("Imported confirmed proof from first sweep")

	// Submit package for second sweep.
	submitRes2, err := btcClient.SubmitPackage(
		[]*wire.MsgTx{anchorTx2}, childTx2, nil,
	)
	require.NoError(t, err)
	require.NotNil(t, submitRes2)
	for txid, res := range submitRes2.TxResults {
		if res.Error != nil {
			t.Fatalf("tx %s rejected: %s", txid, *res.Error)
		}
	}
	t.Logf("Package 2 submitted successfully")

	_, err = builder2.Publish(
		ctx, f.operatorClient.TapdClient, "sweep-2",
		assets.PublishOptions{},
	)
	require.NoError(t, err)

	// Mine second sweep and update proof.
	minedBlocks2 := h.GenerateAndWait(1)
	minedBlock2 := minedBlocks2[0]
	t.Logf("Mined second sweep at height %d", minedBlock2.Header.Height)

	blockHash2, err := chainhash.NewHashFromStr(minedBlock2.Header.Hash)
	require.NoError(t, err)

	rawBlock2, err := rpcClient.GetBlock(blockHash2)
	require.NoError(t, err)

	var txIndex2 int
	anchorTx2Hash := anchorTx2.TxHash()
	for i, tx := range rawBlock2.Transactions {
		txHash := tx.TxHash()
		if txHash.IsEqual(&anchorTx2Hash) {
			txIndex2 = i
			t.Logf("Found anchor2 at index %d", i)
			break
		}
	}

	finalProof2, err := assets.BuildProofFromTransferData(
		transferData2, [][]byte{confirmedProof1}, 0,
		&assets.ProofParams{
			Block:       rawBlock2,
			BlockHeight: uint32(minedBlock2.Header.Height),
			TxIndex:     txIndex2,
		},
	)
	require.NoError(t, err)
	t.Logf("Generated final proof for second sweep")

	_, err = f.operatorClient.TapdClient.ImportProofFile(ctx, finalProof2)
	require.NoError(t, err)
	t.Logf("Imported second proof")

	// ====================================================================
	// Sweep back to wallet.
	// ====================================================================

	builder3 := assets.NewAssetTxBuilder(f.assetID, f.chainParams)

	// Add second sweep's output as input.
	err = builder3.AddAssetInput(assets.InputConfig{
		ProofFile: finalProof2,
		AnchorKey: assets.AnchorKeySpec{
			Mode: assets.AnchorKeyModeStatic,
			Key:  schnorr.SerializePubKey(destKey2.PubKey()),
		},
	})
	require.NoError(t, err)

	// Derive wallet keys.
	walletScriptKey, walletInternalKey, err := f.operatorClient.
		DeriveNewKeys(ctx)
	require.NoError(t, err)

	// Add wallet output.
	err = builder3.AddAssetOutput(assets.OutputConfig{
		Amount: f.asset.Amount,
		AnchorKey: assets.AnchorKeySpec{
			Mode: assets.AnchorKeyModeStatic,
			Key: schnorr.SerializePubKey(
				walletInternalKey.PubKey,
			),
		},
		Script: assets.DirectWalletScript(&walletScriptKey),
	})
	require.NoError(t, err)

	// Add regular anchor with fees.
	anchorSpec3 := assets.NewEphemeralBTCAnchorSpec()
	anchorSpec3.ValueSat = 1_000

	err = builder3.AddBTCAnchor(anchorSpec3)
	require.NoError(t, err)

	// Compile and commit with wallet funding.
	_, err = builder3.Compile(ctx)
	require.NoError(t, err)

	err = builder3.Commit(
		ctx, f.operatorClient.AssetWalletClient,
		assets.CommitOptions{FeeRate: chainfee.SatPerVByte(10)},
	)
	require.NoError(t, err)

	// Sign the asset input with the taproot-tweaked destKey2.
	proofFile2, err := proof.DecodeFile(finalProof2)
	require.NoError(t, err)
	lastProof2, err := proofFile2.LastProof()
	require.NoError(t, err)

	taprootTweak3, err := assets.GenTaprootRootFromProof(lastProof2)
	require.NoError(t, err)

	tweakedKey3 := txscript.TweakTaprootPrivKey(*destKey2, taprootTweak3)

	digest3, err := builder3.GetKeySpendSigHash(0)
	require.NoError(t, err)

	sig3, err := schnorr.Sign(tweakedKey3, digest3[:])
	require.NoError(t, err)

	err = builder3.ApplyKeySpendSignature(0, sig3.Serialize())
	require.NoError(t, err)

	// Finalize and publish.
	_, err = builder3.FinalizeAnchor(ctx, h.LND.WalletKit)
	require.NoError(t, err)

	resp3, err := builder3.Publish(
		ctx, f.operatorClient.TapdClient, "sweep-to-wallet",
		assets.PublishOptions{},
	)
	require.NoError(t, err)
	t.Logf("Published sweep-to-wallet: %s", resp3.Transfer.AnchorTxHash)

	anchorPsbt3 := builder3.AnchorPsbt()
	require.NotNil(t, anchorPsbt3)
	anchorTx3, err := psbt.Extract(anchorPsbt3)
	require.NoError(t, err)
	for i, out := range anchorTx3.TxOut {
		prefix := min(8, len(out.PkScript))
		t.Logf("Wallet sweep output %d: Value=%d ScriptLen=%d Script[0:8]=%x",
			i, out.Value, len(out.PkScript), out.PkScript[:prefix])
	}

	// Mine transaction.
	minedBlocks3 := h.GenerateAndWait(1)
	minedBlock3 := minedBlocks3[0]
	t.Logf("Mined wallet sweep transaction at height %d",
		minedBlock3.Header.Height)

	// Import the confirmed proof so tapd's archive can serve it.
	blockHash3, err := chainhash.NewHashFromStr(minedBlock3.Header.Hash)
	require.NoError(t, err)

	rawBlock3, err := rpcClient.GetBlock(blockHash3)
	require.NoError(t, err)

	walletAnchorHash, err := chainhash.NewHash(resp3.Transfer.AnchorTxHash)
	require.NoError(t, err)

	var (
		txIndex3 int
		found3   bool
	)
	for i, tx := range rawBlock3.Transactions {
		if tx.TxHash() == *walletAnchorHash {
			txIndex3 = i
			found3 = true
			break
		}
	}
	require.True(t, found3, "wallet anchor tx not found in block")

	confirmedProof3, err := builder3.Proof(0, &assets.ProofParams{
		Block:       rawBlock3,
		BlockHeight: uint32(minedBlock3.Header.Height),
		TxIndex:     txIndex3,
	})
	require.NoError(t, err)

	_, err = f.operatorClient.TapdClient.ImportProofFile(ctx, confirmedProof3)
	require.NoError(t, err)
	t.Logf("Imported wallet sweep proof")

	// ====================================================================
	// Register transfer and verify balance.
	// ====================================================================

	// Find output index for wallet output (first output is asset anchor).
	outputIndex := 0

	// Register transfer to make wallet recognize it.
	walletScriptKeyBytes := walletScriptKey.PubKey.SerializeCompressed()
	_, err = f.operatorClient.RegisterTransfer(
		ctx,
		&taprpc.RegisterTransferRequest{
			AssetId:   f.assetID[:],
			ScriptKey: walletScriptKeyBytes,
			Outpoint: &taprpc.OutPoint{
				Txid:        resp3.Transfer.AnchorTxHash,
				OutputIndex: uint32(outputIndex),
			},
		},
	)
	require.NoError(t, err)
	t.Logf("Registered transfer")

	// Wait for balance to update.
	f.operatorClient.WaitForAssetBalance(f.assetID[:], f.asset.Amount)

	// Verify balance explicitly.
	balance, err := f.operatorClient.TapdClient.GetAssetBalance(
		ctx, f.assetID[:],
	)
	require.NoError(t, err)
	require.Equal(t, f.asset.Amount, balance, "wallet balance mismatch")
	t.Logf("Verified wallet balance: %d", balance)
}
