package assets_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/lightninglabs/darepo-client/assets"
	"github.com/lightninglabs/darepo-client/harness"
	"github.com/lightninglabs/taproot-assets/address"
	tapasset "github.com/lightninglabs/taproot-assets/asset"
	"github.com/lightninglabs/taproot-assets/commitment"
	"github.com/lightninglabs/taproot-assets/taprpc"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/require"
)

const (
	defaultTimeout  = 30 * time.Second
	testAssetAmount = 100_000
)

func readTxWitness(witnessSerialized []byte) (wire.TxWitness, error) {
	r := bytes.NewReader(witnessSerialized)

	// first we extract the number of witness elements
	witCount, err := wire.ReadVarInt(r, 0)
	if err != nil {
		return nil, err
	}

	// read each witness item
	witness := make(wire.TxWitness, witCount)
	for i := uint64(0); i < witCount; i++ {
		witness[i], err = wire.ReadVarBytes(
			r, 0, txscript.MaxScriptSize, "witness",
		)
		if err != nil {
			return nil, err
		}
	}

	return witness, nil
}

func csvClosureScript(pub *btcec.PublicKey, delay uint32) assets.ScriptClosure {
	return (&assets.CSVClosure{
		Key:   pub,
		Delay: delay,
	}).ScriptClosure()
}

func checkSigAddScriptClosure(userKey *btcec.PublicKey,
	operatorKey *btcec.PublicKey) assets.ScriptClosure {

	return (&assets.CheckSigAddClosure{
		Key1: userKey,
		Key2: operatorKey,
	}).ScriptClosure()
}

type assetBoardingFixture struct {
	chainParams *address.ChainParams

	alice          *harness.TapdHarness
	aliceClient    *harness.TapClientHarness
	operatorClient *harness.TapClientHarness
	asset          *taprpc.Asset
	assetID        [32]byte

	userKey     *btcec.PrivateKey
	operatorKey *btcec.PrivateKey

	boardingProof *taprpc.ProofFile
	boardingKit   *assets.OnboardingKit
}

func newBoardingFixtureWithAliceBoardingFunded(h *harness.Harness,
	scriptOnly bool, csvDelay uint32) *assetBoardingFixture {

	t := h.T

	chainParams := address.ParamsForChain(
		chaincfg.RegressionNetParams.Name,
	)

	alice := h.NewTapdHarness("alice")
	t.Cleanup(alice.Stop)
	aliceClient := alice.NewTapClientHarness()
	t.Cleanup(aliceClient.Close)

	operatorClient := h.NewTapClientHarness("operator")
	t.Cleanup(operatorClient.Close)

	harness.FundNode(h, h.LND)
	harness.FundNode(h, alice.LND)

	asset := aliceClient.MintAsset(
		"BUX", testAssetAmount, taprpc.AssetType_NORMAL,
	)
	require.NotNil(t, asset)

	var assetID [32]byte
	copy(assetID[:], asset.AssetGenesis.AssetId)

	// Setup MuSig2 keys used for onboarding deposit.
	userInternalPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	userInternalKey := userInternalPriv.PubKey()

	operatorInternalPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	operatorInternalKey := operatorInternalPriv.PubKey()

	// Create onboarding kit based on mode
	var boardingKit *assets.OnboardingKit
	if scriptOnly {
		boardingKit, err = assets.NewScriptOnlyOnboardingKit(
			userInternalKey, operatorInternalKey,
			keychain.KeyLocator{}, assetID, asset.Amount,
			csvDelay, &chainParams,
		)
	} else {
		boardingKit, err = assets.NewOnboardingKit(
			userInternalKey, operatorInternalKey,
			keychain.KeyLocator{}, assetID, asset.Amount,
			csvDelay, &chainParams,
		)
	}
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(t.Context(), defaultTimeout)
	defer cancel()

	onboardingAddr, err := boardingKit.NewOnboardingAddr(
		ctx, aliceClient.TapdClient,
	)
	require.NoError(t, err)

	aliceClient.SendAsset(onboardingAddr.Encoded)
	h.GenerateAndWait(6)

	matcher := func(t *testing.T, transfers []*taprpc.AssetTransfer) (
		*taprpc.AssetTransfer, int) {

		transfer, idx, err := boardingKit.FindMatchingTransfer(
			transfers,
		)
		if err != nil {
			return nil, -1
		}

		return transfer, idx
	}

	boardingTransfer, outIdx := aliceClient.WaitForTransfer(matcher)
	onboardingOutput := boardingTransfer.Outputs[outIdx]

	outpointParts := strings.Split(onboardingOutput.Anchor.Outpoint, ":")
	var anchorOutputIndex uint32
	_, err = fmt.Sscanf(outpointParts[1], "%d", &anchorOutputIndex)
	require.NoError(t, err)

	var proofFile *taprpc.ProofFile
	require.Eventually(t, func() bool {
		proofResp, err := aliceClient.ExportProof(
			ctx, &taprpc.ExportProofRequest{
				AssetId:   onboardingOutput.AssetId,
				ScriptKey: onboardingOutput.ScriptKey,
				Outpoint: &taprpc.OutPoint{
					Txid: boardingTransfer.
						AnchorTxHash,
					OutputIndex: anchorOutputIndex,
				},
			},
		)
		if err != nil {
			return false
		}

		proofFile = proofResp

		return true
	}, defaultTimeout, time.Second)

	return &assetBoardingFixture{
		chainParams:    &chainParams,
		alice:          alice,
		aliceClient:    aliceClient,
		operatorClient: operatorClient,
		asset:          asset,
		assetID:        assetID,
		userKey:        userInternalPriv,
		operatorKey:    operatorInternalPriv,
		boardingProof:  proofFile,
		boardingKit:    boardingKit,
	}
}

func TestAssetBoardingMuSig2Sweep(t *testing.T) {
	opts := harness.DefaultOptions()
	opts.StartTapd = true
	opts.GroupName = "asset-boarding-musig2-sweep"

	h := harness.NewHarness(t, &opts)
	h.Start()
	t.Cleanup(h.Stop)

	const scriptOnly = false
	const csvDelay = uint32(144)
	f := newBoardingFixtureWithAliceBoardingFunded(h, scriptOnly, csvDelay)

	userPubKey := f.userKey.PubKey()
	operatorPubKey := f.operatorKey.PubKey()

	// Destination anchor keys have to be derived by the tapd wallet so it
	// can sign the resulting anchor transaction. Fetch a fresh
	// script/internal key pair from tapd's asset wallet.
	destScriptKey, destInternalKeyDesc, err :=
		f.operatorClient.DeriveNewKeys(
			t.Context(),
		)
	require.NoError(t, err)
	destInternalKey := destInternalKeyDesc.PubKey

	builder := assets.NewAssetTxBuilder(f.assetID, f.chainParams)

	inputMuSig := &assets.MuSig2Spec{
		Participants: []assets.MuSig2Participant{
			{
				Role:   assets.SignerRole("user"),
				PubKey: userPubKey.SerializeCompressed(),
			},
			{
				Role:   assets.SignerRole("operator"),
				PubKey: operatorPubKey.SerializeCompressed(),
			},
		},
		SortKeys: true,
		Tweaks:   assets.MuSig2Tweaks{TaprootBIP0086Tweak: true},
	}

	require.NoError(t, builder.AddAssetInput(assets.InputConfig{
		ProofFile: f.boardingProof.RawProofFile,
		AnchorKey: assets.AnchorKeySpec{
			Mode:   assets.AnchorKeyModeMuSig2,
			MuSig2: inputMuSig,
		},
		Closures: []assets.ScriptClosure{
			csvClosureScript(userPubKey, csvDelay),
		},
	}))

	require.NoError(t, builder.AddAssetOutput(assets.OutputConfig{
		Amount: f.asset.Amount,
		AnchorKey: assets.AnchorKeySpec{
			Mode: assets.AnchorKeyModeStatic,
			Key:  schnorr.SerializePubKey(destInternalKey),
		},
		Script: assets.OpTrueScriptWithWalletKey(
			&destScriptKey, destInternalKey,
		),
	}))

	ctx, cancel := context.WithTimeout(t.Context(), defaultTimeout)
	defer cancel()

	plan, err := builder.Compile(ctx)
	require.NoError(t, err)

	require.Len(t, plan.OutputPlans, 1)
	outputPlan := plan.OutputPlans[0]
	require.NotNil(t, outputPlan.Witness.ScriptDetails)
	require.Equal(t,
		assets.AssetScriptTypeOpTrue,
		outputPlan.Witness.ScriptDetails.Type(),
	)
	scriptDetails := outputPlan.Witness.ScriptDetails
	opDetails, ok := scriptDetails.(*assets.OpTrueScriptDetails)
	require.True(t, ok)
	require.NotNil(t, opDetails.Artifacts)

	commitOpts := assets.CommitOptions{FeeRate: chainfee.SatPerVByte(1)}
	require.NoError(t, builder.Commit(
		ctx, f.operatorClient.AssetWalletClient, commitOpts),
	)

	digest, err := builder.GetKeySpendSigHash(0)
	require.NoError(t, err)

	_, _, taprootRoot, err := builder.GetTaprootRoots(0, "")
	require.NoError(t, err)
	t.Logf("taproot tweak root: %x", taprootRoot)

	// Set up the taproot tweak for MuSig2 signing.
	allPubKeys := []*btcec.PublicKey{userPubKey, operatorPubKey}
	tweaks := &input.MuSig2Tweaks{
		TaprootBIP0086Tweak: false,
		TaprootTweak:        taprootRoot,
	}

	// Create MuSig2 signers and sessions for both parties.
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

	// Both parties create partial signatures. MuSig2Sign registers the
	// signature internally in the session.
	_, err = userSigner.MuSig2Sign(userSession.SessionID, digest, false)
	require.NoError(t, err)

	operatorPartialSig, err := operatorSigner.MuSig2Sign(
		operatorSession.SessionID, digest, false,
	)
	require.NoError(t, err)

	// User side combines signatures to create final signature.
	// (Operator side could also do this and produce the same result.)
	finalSig, haveAll, err := userSigner.MuSig2CombineSig(
		userSession.SessionID,
		[]*musig2.PartialSignature{operatorPartialSig},
	)
	require.NoError(t, err)
	require.True(t, haveAll)

	require.NoError(
		t, builder.ApplyKeySpendSignature(0, finalSig.Serialize()),
	)

	_, err = builder.FinalizeAnchor(ctx, h.LND.WalletKit)
	require.NoError(t, err)

	publishResp, err := builder.Publish(
		ctx, f.operatorClient.TapdClient, "cooperative-sweep",
		assets.PublishOptions{},
	)
	require.NoError(t, err)
	require.NotNil(t, publishResp)

	expectedScriptKeyCompressed := destScriptKey.PubKey.
		SerializeCompressed()
	expectedInternalKey := destInternalKey.SerializeCompressed()
	expectedScriptKeyX, err := harness.NormalizeScriptKey(
		schnorr.SerializePubKey(destScriptKey.PubKey),
	)
	require.NoError(t, err)

	require.NotNil(t, publishResp.Transfer)
	require.NotEmpty(t, publishResp.Transfer.Outputs)
	out := publishResp.Transfer.Outputs[0]
	actualScriptKeyX, err := harness.NormalizeScriptKey(out.ScriptKey)
	require.NoError(t, err)
	require.Equal(t, expectedScriptKeyX, actualScriptKeyX)
	require.Equal(t, expectedInternalKey, out.Anchor.InternalKey)

	artifacts, err := assets.BuildOpTrueArtifacts()
	require.NoError(t, err)

	encodedSibling, _, err := commitment.MaybeEncodeTapscriptPreimage(
		artifacts.SiblingPreimage,
	)
	require.NoError(t, err)
	require.Equal(t, encodedSibling, out.Anchor.TapscriptSibling)

	h.GenerateAndWait(6)

	status := taprpc.
		ProofDeliveryStatus_PROOF_DELIVERY_STATUS_NOT_APPLICABLE
	f.operatorClient.WaitForTransfer(
		harness.MatchAssetTransfer(
			f.assetID[:], expectedScriptKeyCompressed,
			expectedInternalKey, f.asset.Amount, status,
		),
	)
}

func TestAssetBoardingScriptSweep(t *testing.T) {
	opts := harness.DefaultOptions()
	opts.StartTapd = true
	opts.GroupName = "asset-boarding-script-sweep"

	h := harness.NewHarness(t, &opts)
	h.Start()
	t.Cleanup(h.Stop)

	const csvDelay = uint32(144)
	const scriptOnly = true
	f := newBoardingFixtureWithAliceBoardingFunded(h, scriptOnly, csvDelay)

	userPubKey := f.userKey.PubKey()
	operatorPubKey := f.operatorKey.PubKey()

	boardingProof := harness.LatestProofFromBlob(
		t, f.boardingProof.RawProofFile,
	)

	builder := assets.NewAssetTxBuilder(f.assetID, f.chainParams)

	coopClosure := checkSigAddScriptClosure(
		userPubKey, operatorPubKey,
	)
	timeoutClosure := csvClosureScript(userPubKey, csvDelay)

	require.NoError(t, builder.AddAssetInput(assets.InputConfig{
		ProofFile: f.boardingProof.RawProofFile,
		AnchorKey: assets.AnchorKeySpec{
			Mode: assets.AnchorKeyModeStatic,
			Key:  schnorr.SerializePubKey(tapasset.NUMSPubKey),
		},
		Closures: []assets.ScriptClosure{
			coopClosure, timeoutClosure,
		},
	}))

	destScriptKey, destInternalKeyDesc, err :=
		f.operatorClient.DeriveNewKeys(
			t.Context(),
		)
	require.NoError(t, err)
	destInternalKey := destInternalKeyDesc.PubKey

	require.NoError(t, builder.AddAssetOutput(assets.OutputConfig{
		Amount: f.asset.Amount,
		AnchorKey: assets.AnchorKeySpec{
			Mode: assets.AnchorKeyModeStatic,
			Key:  schnorr.SerializePubKey(destInternalKey),
		},
		Script: assets.OpTrueScriptWithWalletKey(
			&destScriptKey, destInternalKey,
		),
	}))

	compileCtx, compileCancel := context.WithTimeout(
		t.Context(), defaultTimeout,
	)
	defer compileCancel()

	plan, err := builder.Compile(compileCtx)
	require.NoError(t, err)

	require.Len(t, plan.OutputPlans, 1)
	outputPlan := plan.OutputPlans[0]
	require.NotNil(t, outputPlan.Witness.ScriptDetails)
	require.Equal(t,
		assets.AssetScriptTypeOpTrue,
		outputPlan.Witness.ScriptDetails.Type(),
	)
	scriptDetails := outputPlan.Witness.ScriptDetails
	opDetails, ok := scriptDetails.(*assets.OpTrueScriptDetails)
	require.True(t, ok)
	require.NotNil(t, opDetails.Artifacts)

	commitOpts := assets.CommitOptions{
		FeeRate: chainfee.SatPerVByte(1),
	}

	require.NoError(t, builder.Commit(
		compileCtx, f.operatorClient.AssetWalletClient, commitOpts,
	))

	assetInputIndex := harness.FindAssetInputIndex(
		t, builder.AnchorPsbt(), boardingProof,
	)

	for idx, txIn := range builder.AnchorPsbt().UnsignedTx.TxIn {
		t.Logf("psbt input %d: %s", idx, txIn.PreviousOutPoint.String())
	}
	t.Logf("asset input index: %d", assetInputIndex)

	scriptSpend, err := builder.PrepareScriptSpend(
		assetInputIndex, "coop_multisig",
	)
	require.NoError(t, err)
	t.Logf("script spend digest: %x", scriptSpend.SigHash[:])
	t.Logf("script root: %x", scriptSpend.ScriptRoot)
	t.Logf("taproot root: %x", scriptSpend.TaprootRoot)

	require.NotEmpty(t, scriptSpend.ControlBlock)
	require.NotNil(t, scriptSpend.OutputKey)

	coopLeaf := scriptSpend.TapLeaf

	timeoutLeaf, err := timeoutClosure.TapLeaf()
	require.NoError(t, err)
	coopDisasm, err := txscript.DisasmString(coopLeaf.Script)
	require.NoError(t, err)
	t.Logf("coop script: %s", coopDisasm)

	require.Len(t, scriptSpend.AssetRoot, 32)
	proofAssetRoot, err := assets.GenTaprootAssetRootFromProof(
		boardingProof,
	)
	require.NoError(t, err)
	require.Equal(t, proofAssetRoot, scriptSpend.AssetRoot)

	timeoutHash := timeoutLeaf.TapHash()
	controlBlock := &txscript.ControlBlock{
		InternalKey:    scriptSpend.InternalKey,
		LeafVersion:    scriptSpend.TapLeaf.LeafVersion,
		InclusionProof: append([]byte(nil), timeoutHash[:]...),
	}
	controlBlock.InclusionProof = append(
		controlBlock.InclusionProof, scriptSpend.AssetRoot...,
	)
	if scriptSpend.OutputKey.SerializeCompressed()[0]&1 == 1 {
		controlBlock.OutputKeyYIsOdd = true
	}
	controlBlockBytes, err := controlBlock.ToBytes()
	require.NoError(t, err)
	require.Equal(t, controlBlockBytes, scriptSpend.ControlBlock)

	userSig, err := schnorr.Sign(f.userKey, scriptSpend.SigHash[:])
	require.NoError(t, err)
	require.True(t, userSig.Verify(scriptSpend.SigHash[:], userPubKey))

	operatorSig, err := schnorr.Sign(
		f.operatorKey, scriptSpend.SigHash[:],
	)
	require.NoError(t, err)
	require.True(t,
		operatorSig.Verify(scriptSpend.SigHash[:], operatorPubKey),
	)

	operatorKeyHex := hex.EncodeToString(
		schnorr.SerializePubKey(operatorPubKey),
	)
	userKeyHex := hex.EncodeToString(
		schnorr.SerializePubKey(userPubKey),
	)
	sigMap := map[string][]byte{
		operatorKeyHex: operatorSig.Serialize(),
		userKeyHex:     userSig.Serialize(),
	}

	require.NoError(t, builder.ApplyScriptSpend(scriptSpend, sigMap))

	psbtInput := builder.AnchorPsbt().Inputs[assetInputIndex]
	parsedWitness, err := readTxWitness(
		psbtInput.FinalScriptWitness,
	)
	require.NoError(t, err)

	manualWitness := wire.TxWitness{
		operatorSig.Serialize(),
		userSig.Serialize(),
		coopLeaf.Script,
		controlBlockBytes,
	}
	require.Equal(t, manualWitness, parsedWitness)

	finalizeCtx, finalizeCancel := context.WithTimeout(
		t.Context(), defaultTimeout,
	)
	defer finalizeCancel()

	_, err = builder.FinalizeAnchor(finalizeCtx, h.LND.WalletKit)
	require.NoError(t, err)

	anchorPsbt := builder.AnchorPsbt()
	finalTx := anchorPsbt.UnsignedTx.Copy()

	prevOuts := make(map[wire.OutPoint]*wire.TxOut)
	for i := range finalTx.TxIn {
		var utxo *wire.TxOut
		input := anchorPsbt.Inputs[i]
		switch {
		case input.WitnessUtxo != nil:
			utxo = input.WitnessUtxo
		case input.NonWitnessUtxo != nil:
			prevIdx := finalTx.TxIn[i].PreviousOutPoint.Index
			utxo = input.NonWitnessUtxo.TxOut[prevIdx]
		}
		if utxo != nil {
			prevOuts[finalTx.TxIn[i].PreviousOutPoint] = utxo
		}
	}
	fetcher := txscript.NewMultiPrevOutFetcher(prevOuts)
	hashCache := txscript.NewTxSigHashes(finalTx, fetcher)

	outputIndex := boardingProof.InclusionProof.OutputIndex
	anchorOutput := boardingProof.AnchorTx.TxOut[outputIndex]
	require.Len(t, anchorOutput.PkScript, 34)
	wp := anchorOutput.PkScript[2:]
	t.Logf("anchor witness program: %x", wp)

	engine, err := txscript.NewEngine(
		anchorOutput.PkScript, finalTx, assetInputIndex,
		txscript.StandardVerifyFlags|txscript.ScriptVerifyTaproot,
		nil, hashCache, anchorOutput.Value, fetcher,
	)
	require.NoError(t, err)
	require.NoError(t, engine.Execute())

	publishCtx, publishCancel := context.WithTimeout(
		t.Context(), defaultTimeout,
	)
	defer publishCancel()

	publishResp, err := builder.Publish(
		publishCtx, f.operatorClient.TapdClient, "script-only-sweep",
		assets.PublishOptions{},
	)
	require.NoError(t, err)

	require.NotNil(t, publishResp)
	require.NotNil(t, publishResp.Transfer)
	require.Len(t, publishResp.Transfer.Outputs, 1)

	expectedScriptKeyCompressed := destScriptKey.PubKey.
		SerializeCompressed()
	expectedInternalKey := destInternalKey.SerializeCompressed()
	expectedScriptKeyX, err := harness.NormalizeScriptKey(
		schnorr.SerializePubKey(destScriptKey.PubKey),
	)
	require.NoError(t, err)

	out := publishResp.Transfer.Outputs[0]
	actualScriptKeyX, err := harness.NormalizeScriptKey(out.ScriptKey)
	require.NoError(t, err)
	require.Equal(t, expectedScriptKeyX, actualScriptKeyX)
	require.Equal(t, expectedInternalKey, out.Anchor.InternalKey)

	artifacts, err := assets.BuildOpTrueArtifacts()
	require.NoError(t, err)

	encodedSibling, _, err := commitment.MaybeEncodeTapscriptPreimage(
		artifacts.SiblingPreimage,
	)
	require.NoError(t, err)
	require.Equal(t, encodedSibling, out.Anchor.TapscriptSibling)

	h.GenerateAndWait(6)

	status :=
		taprpc.ProofDeliveryStatus_PROOF_DELIVERY_STATUS_NOT_APPLICABLE
	f.operatorClient.WaitForTransfer(
		harness.MatchAssetTransfer(
			f.assetID[:], expectedScriptKeyCompressed,
			expectedInternalKey, f.asset.Amount, status,
		),
	)
}

func TestAssetBoardingTimeoutSweep(t *testing.T) {
	opts := harness.DefaultOptions()
	opts.StartTapd = true
	opts.GroupName = "asset-boarding-timeout-sweep"

	h := harness.NewHarness(t, &opts)
	h.Start()
	t.Cleanup(h.Stop)

	const scriptOnly = true
	const csvDelay = uint32(2)
	f := newBoardingFixtureWithAliceBoardingFunded(h, scriptOnly, csvDelay)

	userPubKey := f.userKey.PubKey()
	operatorPubKey := f.operatorKey.PubKey()

	boardingProof := harness.LatestProofFromBlob(
		t, f.boardingProof.RawProofFile,
	)

	builder := assets.NewAssetTxBuilder(f.assetID, f.chainParams)

	coopClosure := checkSigAddScriptClosure(userPubKey, operatorPubKey)
	timeoutClosure := csvClosureScript(userPubKey, csvDelay)

	require.NoError(t, builder.AddAssetInput(assets.InputConfig{
		ProofFile: f.boardingProof.RawProofFile,
		AnchorKey: assets.AnchorKeySpec{
			Mode: assets.AnchorKeyModeStatic,
			Key:  schnorr.SerializePubKey(tapasset.NUMSPubKey),
		},
		Closures: []assets.ScriptClosure{
			coopClosure, timeoutClosure,
		},
	}))

	// Alice (user) derives destination keys for the timeout sweep.
	destScriptKey, destInternalKeyDesc, err :=
		f.aliceClient.DeriveNewKeys(
			t.Context(),
		)
	require.NoError(t, err)
	destInternalKey := destInternalKeyDesc.PubKey

	require.NoError(t, builder.AddAssetOutput(assets.OutputConfig{
		Amount: f.asset.Amount,
		AnchorKey: assets.AnchorKeySpec{
			Mode: assets.AnchorKeyModeStatic,
			Key:  schnorr.SerializePubKey(destInternalKey),
		},
		Script: assets.OpTrueScriptWithWalletKey(
			&destScriptKey, destInternalKey,
		),
	}))

	compileCtx, compileCancel := context.WithTimeout(
		t.Context(), defaultTimeout,
	)
	defer compileCancel()

	plan, err := builder.Compile(compileCtx)
	require.NoError(t, err)

	require.Len(t, plan.OutputPlans, 1)
	outputPlan := plan.OutputPlans[0]
	require.NotNil(t, outputPlan.Witness.ScriptDetails)
	require.Equal(t,
		assets.AssetScriptTypeOpTrue,
		outputPlan.Witness.ScriptDetails.Type(),
	)
	scriptDetails := outputPlan.Witness.ScriptDetails
	opDetails, ok := scriptDetails.(*assets.OpTrueScriptDetails)
	require.True(t, ok)
	require.NotNil(t, opDetails.Artifacts)

	commitOpts := assets.CommitOptions{
		FeeRate: chainfee.SatPerVByte(1),
	}

	// Alice builds and commits the timeout sweep
	require.NoError(t, builder.Commit(
		compileCtx, f.aliceClient.AssetWalletClient, commitOpts,
	))

	assetInputIndex := harness.FindAssetInputIndex(
		t, builder.AnchorPsbt(), boardingProof,
	)

	anchorPsbt := builder.AnchorPsbt()
	anchorPsbt.UnsignedTx.TxIn[assetInputIndex].Sequence = csvDelay

	scriptSpend, err := builder.PrepareScriptSpend(assetInputIndex, "csv")
	require.NoError(t, err)
	t.Logf("timeout script spend digest: %x", scriptSpend.SigHash[:])

	require.NotEmpty(t, scriptSpend.ControlBlock)
	require.NotNil(t, scriptSpend.OutputKey)

	coopLeaf, err := coopClosure.TapLeaf()
	require.NoError(t, err)

	require.Len(t, scriptSpend.AssetRoot, 32)

	coopHash := coopLeaf.TapHash()
	controlBlock := &txscript.ControlBlock{
		InternalKey:    scriptSpend.InternalKey,
		LeafVersion:    scriptSpend.TapLeaf.LeafVersion,
		InclusionProof: append([]byte(nil), coopHash[:]...),
	}
	controlBlock.InclusionProof = append(
		controlBlock.InclusionProof, scriptSpend.AssetRoot...,
	)
	if scriptSpend.OutputKey.SerializeCompressed()[0]&1 == 1 {
		controlBlock.OutputKeyYIsOdd = true
	}
	controlBlockBytes, err := controlBlock.ToBytes()
	require.NoError(t, err)
	require.Equal(t, controlBlockBytes, scriptSpend.ControlBlock)

	userSig, err := schnorr.Sign(f.userKey, scriptSpend.SigHash[:])
	require.NoError(t, err)
	require.True(t, userSig.Verify(scriptSpend.SigHash[:], userPubKey))

	userKeyHex := hex.EncodeToString(schnorr.SerializePubKey(userPubKey))
	sigMap := map[string][]byte{
		userKeyHex: userSig.Serialize(),
	}

	require.NoError(t, builder.ApplyScriptSpend(scriptSpend, sigMap))

	psbtInput := builder.AnchorPsbt().Inputs[assetInputIndex]
	parsedWitness, err := readTxWitness(
		psbtInput.FinalScriptWitness,
	)
	require.NoError(t, err)

	timeoutScript, err := timeoutClosure.Script()
	require.NoError(t, err)
	manualWitness := wire.TxWitness{
		userSig.Serialize(),
		timeoutScript,
		controlBlockBytes,
	}
	require.Equal(t, manualWitness, parsedWitness)

	finalizeCtx, finalizeCancel := context.WithTimeout(
		t.Context(), defaultTimeout,
	)
	defer finalizeCancel()

	// Alice finalizes the anchor PSBT using her wallet.
	_, err = builder.FinalizeAnchor(finalizeCtx, f.alice.LND.WalletKit)
	require.NoError(t, err)

	publishCtx, publishCancel := context.WithTimeout(
		t.Context(), defaultTimeout,
	)
	defer publishCancel()

	// Alice publishes the timeout sweep.
	publishResp, err := builder.Publish(
		publishCtx, f.aliceClient.TapdClient,
		"script-only-timeout-sweep",
		assets.PublishOptions{},
	)
	require.NoError(t, err)

	require.NotNil(t, publishResp)
	require.NotNil(t, publishResp.Transfer)
	require.Len(t, publishResp.Transfer.Outputs, 1)

	expectedScriptKeyCompressed := destScriptKey.PubKey.
		SerializeCompressed()
	expectedInternalKey := destInternalKey.SerializeCompressed()
	expectedScriptKeyX, err := harness.NormalizeScriptKey(
		schnorr.SerializePubKey(destScriptKey.PubKey),
	)
	require.NoError(t, err)

	out := publishResp.Transfer.Outputs[0]
	actualScriptKeyX, err := harness.NormalizeScriptKey(out.ScriptKey)
	require.NoError(t, err)
	require.Equal(t, expectedScriptKeyX, actualScriptKeyX)
	require.Equal(t, expectedInternalKey, out.Anchor.InternalKey)

	artifacts, err := assets.BuildOpTrueArtifacts()
	require.NoError(t, err)

	encodedSibling, _, err := commitment.MaybeEncodeTapscriptPreimage(
		artifacts.SiblingPreimage,
	)
	require.NoError(t, err)
	require.Equal(t, encodedSibling, out.Anchor.TapscriptSibling)

	h.GenerateAndWait(6)

	status :=
		taprpc.ProofDeliveryStatus_PROOF_DELIVERY_STATUS_NOT_APPLICABLE
	f.aliceClient.WaitForTransfer(
		harness.MatchAssetTransfer(
			f.asset.AssetGenesis.AssetId,
			expectedScriptKeyCompressed, expectedInternalKey,
			f.asset.Amount, status,
		),
	)
}

func TestAssetBtcHybridSpend(t *testing.T) {
	opts := harness.DefaultOptions()
	opts.StartTapd = true
	opts.GroupName = "asset-btc-hybrid"

	h := harness.NewHarness(t, &opts)
	h.Start()
	t.Cleanup(h.Stop)

	const scriptOnly = false
	const csvDelay = uint32(144)
	f := newBoardingFixtureWithAliceBoardingFunded(h, scriptOnly, csvDelay)

	ctx, cancel := context.WithTimeout(t.Context(), defaultTimeout)
	defer cancel()

	builder := assets.NewAssetTxBuilder(f.assetID, f.chainParams)

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

	require.NoError(t, builder.AddAssetInput(assets.InputConfig{
		ProofFile: f.boardingProof.RawProofFile,
		AnchorKey: assets.AnchorKeySpec{
			Mode:   assets.AnchorKeyModeMuSig2,
			MuSig2: inputMuSig,
		},
		Closures: []assets.ScriptClosure{
			csvClosureScript(f.userKey.PubKey(), csvDelay),
		},
	}))

	destScriptKey, destInternalKeyDesc, err :=
		f.operatorClient.DeriveNewKeys(t.Context())
	require.NoError(t, err)

	require.NoError(t, builder.AddAssetOutput(assets.OutputConfig{
		Amount: f.asset.Amount,
		AnchorKey: assets.AnchorKeySpec{
			Mode: assets.AnchorKeyModeStatic,
			Key: schnorr.SerializePubKey(
				destInternalKeyDesc.PubKey,
			),
		},
		Script: assets.OpTrueScriptWithWalletKey(
			&destScriptKey, destInternalKeyDesc.PubKey,
		),
	}))

	btcClient, err := h.BitcoinRPCClient()
	require.NoError(t, err)
	defer btcClient.Shutdown()

	internalPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	internalKey := internalPriv.PubKey()

	scriptBytes, err := txscript.NewScriptBuilder().
		AddOp(txscript.OP_TRUE).
		Script()
	require.NoError(t, err)

	tapLeaf := txscript.NewBaseTapLeaf(scriptBytes)
	rootHash := tapLeaf.TapHash()
	taprootKey := txscript.ComputeTaprootOutputKey(
		internalKey, rootHash[:],
	)

	controlBlock := &txscript.ControlBlock{
		InternalKey: internalKey,
		LeafVersion: txscript.BaseLeafVersion,
	}
	if taprootKey.SerializeCompressed()[0] ==
		secp256k1.PubKeyFormatCompressedOdd {

		controlBlock.OutputKeyYIsOdd = true
	}
	controlBlockBytes, err := controlBlock.ToBytes()
	require.NoError(t, err)

	btcAddr, err := btcutil.NewAddressTaproot(
		schnorr.SerializePubKey(taprootKey),
		&chaincfg.RegressionNetParams,
	)
	require.NoError(t, err)
	btcScript, err := txscript.PayToAddrScript(btcAddr)
	require.NoError(t, err)

	const btcInputValue = 20_000
	txHash, err := btcClient.SendToAddress(
		btcAddr, btcutil.Amount(btcInputValue),
	)
	require.NoError(t, err)

	h.Generate(1)

	rawTx, err := btcClient.GetRawTransaction(txHash)
	require.NoError(t, err)

	var (
		btcOutIndex = -1
	)
	for idx, txOut := range rawTx.MsgTx().TxOut {
		if bytes.Equal(txOut.PkScript, btcScript) {
			btcOutIndex = idx
			break
		}
	}
	require.NotEqual(t, -1, btcOutIndex, "expected btc utxo not found")

	btcOutpoint := wire.OutPoint{
		Hash:  *rawTx.Hash(),
		Index: uint32(btcOutIndex),
	}
	require.NoError(t, builder.AddBtcInput(assets.BtcInputSpec{
		Description: "wallet-utxo",
		Outpoint:    btcOutpoint,
		WitnessUtxo: &wire.TxOut{
			Value:    rawTx.MsgTx().TxOut[btcOutIndex].Value,
			PkScript: append([]byte(nil), btcScript...),
		},
		TaprootLeafScript: []*psbt.TaprootTapLeafScript{
			{
				ControlBlock: controlBlockBytes,
				Script: append(
					[]byte(nil), scriptBytes...,
				),
				LeafVersion: txscript.BaseLeafVersion,
			},
		},
	}))

	destKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	destAddr, err := btcutil.NewAddressTaproot(
		schnorr.SerializePubKey(destKey.PubKey()),
		&chaincfg.RegressionNetParams,
	)
	require.NoError(t, err)
	destScript, err := txscript.PayToAddrScript(destAddr)
	require.NoError(t, err)

	changeKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	changeAddr, err := btcutil.NewAddressTaproot(
		schnorr.SerializePubKey(changeKey.PubKey()),
		&chaincfg.RegressionNetParams,
	)
	require.NoError(t, err)
	changeScript, err := txscript.PayToAddrScript(changeAddr)
	require.NoError(t, err)

	require.NoError(t, builder.AddBtcOutput(assets.BtcOutputSpec{
		Description: "dest",
		ValueSat:    9_000,
		PkScript:    destScript,
	}))

	require.NoError(t, builder.AddBtcOutput(assets.BtcOutputSpec{
		Description: "change",
		ValueSat:    8_000,
		PkScript:    changeScript,
	}))

	plan, err := builder.Compile(ctx)
	require.NoError(t, err)
	require.Len(t, plan.BtcInputs, 1)
	require.Len(t, plan.BtcOutputs, 2)

	require.NoError(t, builder.Commit(
		ctx, f.operatorClient.AssetWalletClient,
		assets.CommitOptions{SkipWalletFunding: true},
	))

	btcInputs := builder.BtcInputs()
	require.Len(t, btcInputs, 1)
	require.Equal(t, btcOutpoint, btcInputs[0].Outpoint)

	btcOutputs := builder.BtcOutputs()
	require.Len(t, btcOutputs, 2)

	anchorPsbt := builder.AnchorPsbt()
	require.NotNil(t, anchorPsbt)

	foundBtcInput := false
	for idx, in := range anchorPsbt.UnsignedTx.TxIn {
		if in.PreviousOutPoint == btcOutpoint {
			foundBtcInput = true
			require.Len(
				t, anchorPsbt.Inputs[idx].TaprootLeafScript, 1,
			)
			tapData := anchorPsbt.Inputs[idx].TaprootLeafScript[0]
			require.Equal(t, scriptBytes, tapData.Script)
			require.Equal(
				t, controlBlockBytes, tapData.ControlBlock,
			)
			require.Equal(
				t, txscript.BaseLeafVersion,
				tapData.LeafVersion,
			)

			break
		}
	}
	require.True(t, foundBtcInput, "btc input missing from anchor psbt")

	destMatched := false
	changeMatched := false
	for idx, txOut := range anchorPsbt.UnsignedTx.TxOut {
		switch {
		case bytes.Equal(txOut.PkScript, destScript):
			destMatched = true
			require.Equal(t, int64(9_000), txOut.Value)
			require.Equal(t, idx, btcOutputs[0].OutputIndex)

		case bytes.Equal(txOut.PkScript, changeScript):
			changeMatched = true
			require.Equal(t, int64(8_000), txOut.Value)
			require.Equal(t, idx, btcOutputs[1].OutputIndex)
		}
	}

	require.True(t, destMatched, "dest output missing from anchor psbt")
	require.True(t, changeMatched, "change output missing from anchor psbt")
}
