package oor

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	oortx "github.com/lightninglabs/wavelength/lib/tx/oor"
	"github.com/stretchr/testify/require"
)

// TestPreparedSubmitPackageRejectsInvalidInputs covers mismatch and binding
// failure paths in PreparedSubmitPackage.AttachTapLeafScripts and Validate.
func TestPreparedSubmitPackageRejectsInvalidInputs(t *testing.T) {
	t.Parallel()

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	receiverKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	inputRoot := chainhash.Hash{0x10}
	cpRoot := chainhash.Hash{0x20}
	recRoot := chainhash.Hash{0x30}

	in := newTestComposedAssetInput(
		t, clientKey, operatorKey.PubKey(), wire.OutPoint{
			Hash:  [32]byte{0x05},
			Index: 0,
		},
		2_000,
		inputRoot, "asset-ref",
		50,
		oortx.CarrierOriginRound,
	)
	require.NoError(t, in.Validate())

	// An asset input with an unspecified carrier origin or a mismatched
	// pkScript/root is rejected by TransferInput.Validate.
	badOrigin := in
	badOrigin.TaprootAssetCarrierOrigin = oortx.CarrierOriginUnspecified
	require.ErrorContains(t, badOrigin.Validate(), "carrier origin")

	badRootIn := in
	badVTXO := *in.VTXO
	wrongInputRoot := chainhash.Hash{0xfe}
	badVTXO.TaprootAssetRoot = &wrongInputRoot
	badRootIn.VTXO = &badVTXO
	require.ErrorContains(t, badRootIn.Validate(), "pkscript mismatch")

	cpPolicy := arkscript.CheckpointPolicy{
		OperatorKey: operatorKey.PubKey(),
		CSVDelay:    10,
	}
	cpIn, err := in.CheckpointInput()
	require.NoError(t, err)
	cpArt, err := oortx.BuildCheckpointPSBT(cpPolicy, cpIn)
	require.NoError(t, err)

	cpTapscript, err := arkscript.CheckpointTapScript(
		cpPolicy, cpArt.OwnerLeafScript,
	)
	require.NoError(t, err)
	cpLeaves := make([]arkscript.PolicyLeaf, len(cpTapscript.Leaves))
	for i, l := range cpTapscript.Leaves {
		cpLeaves[i] = arkscript.PolicyLeaf{Leaf: l}
	}
	cpCompiled, err := arkscript.BuildTree(cpLeaves, &arkscript.ARKNUMSKey)
	require.NoError(t, err)
	cpComposed, err := arkscript.ComposeWithSiblingRoot(cpCompiled, cpRoot)
	require.NoError(t, err)
	cpScript, err := txscript.PayToTaprootScript(cpComposed.OutputKey())
	require.NoError(t, err)

	composedCPTx := cpArt.PSBT.UnsignedTx.Copy()
	composedCPTx.TxOut[0].PkScript = cpScript
	composedCP, err := psbt.NewFromUnsignedTx(composedCPTx)
	require.NoError(t, err)
	composedCP.Inputs[0].WitnessUtxo = cpArt.PSBT.Inputs[0].WitnessUtxo
	composedCP.Outputs[0].TaprootTapTree = cpArt.TapTreeEncoded
	cpArt.PSBT = composedCP

	cpOut, err := cpArt.ToCheckpointOutput()
	require.NoError(t, err)

	rec := newTestComposedAssetRecipient(
		t, receiverKey.PubKey(), operatorKey.PubKey(),
		btcutil.Amount(2_000), recRoot, "asset-ref", 50,
	)

	makeValid := func(t *testing.T) *PreparedSubmitPackage {
		t.Helper()

		arkPSBT, err := oortx.BuildArkPSBT(
			[]oortx.CheckpointOutput{cpOut},
			[]oortx.RecipientOutput{rec},
		)
		require.NoError(t, err)

		return &PreparedSubmitPackage{
			ArkPSBT: arkPSBT,
			CheckpointPSBTs: []*psbt.Packet{
				composedCP,
			},
			CheckpointAssetRoots: []*chainhash.Hash{
				&cpRoot,
			},
			TaprootAssetTransfer: &oortx.TaprootAssetTransfer{
				Version: oortx.TaprootAssetTransferVersion,
				CheckpointPackages: [][]byte{
					[]byte("cp-pkg"),
				},
				ArkPackage: []byte("ark-pkg"),
			},
		}
	}

	// Wrong checkpoint sibling root fails tapleaf binding in
	// AttachTapLeafScripts.
	wrongRoot := chainhash.Hash{0x99}
	badAttach := makeValid(t)
	badAttach.CheckpointAssetRoots = []*chainhash.Hash{&wrongRoot}
	require.ErrorContains(
		t,
		badAttach.AttachTapLeafScripts(
			[]TransferInput{in},
		),
		"does not bind to checkpoint 0 pkScript",
	)

	// Uncomposed tapleaf attached manually fails Validate binding check.
	uncomposed := makeValid(t)
	uncomposedLeaf, err := oortx.BuildTaprootTapLeafScript(
		cpArt.TapTreeEncoded, in.OwnerLeafScript,
	)
	require.NoError(t, err)
	addTaprootLeafScript(&uncomposed.ArkPSBT.Inputs[0], uncomposedLeaf)
	require.ErrorContains(
		t,
		uncomposed.Validate(
			cpPolicy, []TransferInput{in},
			[]oortx.RecipientOutput{rec},
		),
		"does not bind to witness utxo",
	)

	// Valid attach followed by Validate succeeds.
	okPkg := makeValid(t)
	require.NoError(t, okPkg.AttachTapLeafScripts([]TransferInput{in}))
	require.NoError(
		t,
		okPkg.Validate(
			cpPolicy, []TransferInput{in},
			[]oortx.RecipientOutput{rec},
		),
	)

	// Wrong session checkpoint policy (CSV delay or operator key) fails
	// Validate.
	wrongDelayPolicy := cpPolicy
	wrongDelayPolicy.CSVDelay++
	require.ErrorContains(
		t,
		okPkg.Validate(
			wrongDelayPolicy, []TransferInput{in},
			[]oortx.RecipientOutput{rec},
		),
		"does not match session checkpoint policy",
	)

	// Wrong recipient value or script fails Validate.
	wrongValRec := rec
	wrongValRec.Value = 1_999
	require.ErrorContains(
		t,
		okPkg.Validate(
			cpPolicy, []TransferInput{in},
			[]oortx.RecipientOutput{wrongValRec},
		),
		"recipient 0 output mismatch",
	)

	// Non-fee-less Ark transaction fails Validate (build a valid Ark tx
	// and shave 1 sat off the recipient output so it leaks a fee).
	feePkg := makeValid(t)
	feeArkTx := feePkg.ArkPSBT.UnsignedTx.Copy()
	feeArkTx.TxOut[0].Value = int64(wrongValRec.Value)
	feeArk, err := psbt.NewFromUnsignedTx(feeArkTx)
	require.NoError(t, err)
	feeArk.Inputs[0].WitnessUtxo = feePkg.ArkPSBT.Inputs[0].WitnessUtxo
	feePkg.ArkPSBT = feeArk
	require.NoError(t, feePkg.AttachTapLeafScripts([]TransferInput{in}))
	require.ErrorContains(
		t,
		feePkg.Validate(
			cpPolicy, []TransferInput{in},
			[]oortx.RecipientOutput{wrongValRec},
		),
		"not fee-less",
	)

	// Checkpoint whose output value differs from the input VTXO amount
	// fails Validate.
	shortCPTx := composedCP.UnsignedTx.Copy()
	shortCPTx.TxOut[0].Value = 1_900
	shortCP, err := psbt.NewFromUnsignedTx(shortCPTx)
	require.NoError(t, err)
	shortCP.Inputs[0].WitnessUtxo = composedCP.Inputs[0].WitnessUtxo
	shortCP.Outputs[0].TaprootTapTree = cpArt.TapTreeEncoded
	shortCPOut := cpOut
	shortCPOut.Txid = shortCP.UnsignedTx.TxHash()
	shortCPOut.Output = &wire.TxOut{
		Value:    1_900,
		PkScript: shortCPTx.TxOut[0].PkScript,
	}
	shortRec := rec
	shortRec.Value = 1_900
	shortArk, err := oortx.BuildArkPSBT(
		[]oortx.CheckpointOutput{shortCPOut},
		[]oortx.RecipientOutput{shortRec},
	)
	require.NoError(t, err)
	shortPkg := &PreparedSubmitPackage{
		ArkPSBT: shortArk,
		CheckpointPSBTs: []*psbt.Packet{
			shortCP,
		},
		CheckpointAssetRoots: []*chainhash.Hash{
			&cpRoot,
		},
		TaprootAssetTransfer: okPkg.TaprootAssetTransfer.Clone(),
	}
	require.NoError(t, shortPkg.AttachTapLeafScripts([]TransferInput{in}))
	require.ErrorContains(
		t,
		shortPkg.Validate(
			cpPolicy, []TransferInput{in},
			[]oortx.RecipientOutput{shortRec},
		),
		"output value 1900 does not match input amount 2000",
	)

	// Dropping asset metadata from recipients fails Validate (requires at
	// least one asset-bearing output).
	plainRec := rec
	plainRec.Asset = nil
	require.ErrorContains(
		t,
		okPkg.Validate(
			cpPolicy, []TransferInput{in},
			[]oortx.RecipientOutput{plainRec},
		),
		"require at least one asset-bearing output",
	)

	// Checkpoint asset root presence mismatch fails Validate.
	missingRoot := makeValid(t)
	require.NoError(
		t,
		missingRoot.AttachTapLeafScripts(
			[]TransferInput{in},
		),
	)
	missingRoot.CheckpointAssetRoots = []*chainhash.Hash{nil}
	require.ErrorContains(
		t,
		missingRoot.Validate(
			cpPolicy, []TransferInput{in},
			[]oortx.RecipientOutput{rec},
		),
		"checkpoint asset root presence mismatch",
	)

	// Idle.ProcessEvent clones the prepared package on entry so caller
	// PSBTs are never mutated or aliased by FSM state.
	callerPkg := makeValid(t)
	require.Empty(t, callerPkg.ArkPSBT.Inputs[0].TaprootLeafScript)
	tr, err := (&Idle{}).ProcessEvent(
		context.Background(), &StartTransferEvent{
			VTXOInputs:       []TransferInput{in},
			RecipientOutputs: []oortx.RecipientOutput{rec},
			Policy:           cpPolicy,
			PreparedSubmit:   callerPkg,
		}, nil,
	)
	require.NoError(t, err)
	require.Empty(t, callerPkg.ArkPSBT.Inputs[0].TaprootLeafScript)
	awaitArk, ok := tr.NextState.(*AwaitingArkSignatures)
	require.True(t, ok)
	require.NotEmpty(t, awaitArk.ArkPSBT.Inputs[0].TaprootLeafScript)
	require.NotSame(t, callerPkg.ArkPSBT, awaitArk.ArkPSBT)
}

// TestPreparedSubmitTwoInputMisalignmentRejections covers duplicate outpoints,
// swapped checkpoints, and swapped container slots across a two-input package.
func TestPreparedSubmitTwoInputMisalignmentRejections(t *testing.T) {
	t.Parallel()

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	receiverKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	floatKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	cpPolicy := arkscript.CheckpointPolicy{
		OperatorKey: operatorKey.PubKey(),
		CSVDelay:    10,
	}

	assetIn := newTestComposedAssetInput(
		t, clientKey, operatorKey.PubKey(), wire.OutPoint{
			Hash:  [32]byte{0x11},
			Index: 0,
		},
		2_000, chainhash.Hash{0x10}, "asset-ref", 50,
		oortx.CarrierOriginRound,
	)
	require.NoError(t, assetIn.Validate())

	floatPolicy, floatScript, err := arkscript.EncodeStandardVTXOArtifacts(
		floatKey.PubKey(), operatorKey.PubKey(), 144,
	)
	require.NoError(t, err)
	lease := &OORCarrierLease{
		Outpoint: wire.OutPoint{
			Hash: [32]byte{
				0x22,
			},
			Index: 0,
		},
		Value:          5_000,
		PolicyTemplate: floatPolicy,
		PkScript:       floatScript,
		ExpiresAtUnix:  1_800_000_000,
	}
	floatIn, err := lease.ToTransferInput(
		operatorKey.PubKey(), floatKey.PubKey(),
	)
	require.NoError(t, err)

	assetCPIn, err := assetIn.CheckpointInput()
	require.NoError(t, err)
	assetCPArt, err := oortx.BuildCheckpointPSBT(cpPolicy, assetCPIn)
	require.NoError(t, err)

	cpRoot := chainhash.Hash{0x20}
	wantAssetScript, err := expectedCheckpointPkScript(
		cpPolicy, assetCPArt.OwnerLeafScript, &cpRoot,
	)
	require.NoError(t, err)
	composedCPTx := assetCPArt.PSBT.UnsignedTx.Copy()
	composedCPTx.TxOut[0].PkScript = wantAssetScript
	assetCP, err := psbt.NewFromUnsignedTx(composedCPTx)
	require.NoError(t, err)
	assetCP.Inputs[0].WitnessUtxo = assetCPArt.PSBT.Inputs[0].WitnessUtxo
	assetCP.Outputs[0].TaprootTapTree = assetCPArt.TapTreeEncoded
	assetCPArt.PSBT = assetCP

	floatCPIn, err := floatIn.CheckpointInput()
	require.NoError(t, err)
	floatCPArt, err := oortx.BuildCheckpointPSBT(cpPolicy, floatCPIn)
	require.NoError(t, err)

	assetCPOut, err := assetCPArt.ToCheckpointOutput()
	require.NoError(t, err)
	floatCPOut, err := floatCPArt.ToCheckpointOutput()
	require.NoError(t, err)

	recAsset := newTestComposedAssetRecipient(
		t, receiverKey.PubKey(), operatorKey.PubKey(), 1_000,
		chainhash.Hash{0x30}, "asset-ref", 50,
	)
	recChange := oortx.RecipientOutput{
		PkScript:           floatScript,
		Value:              6_000,
		VTXOPolicyTemplate: floatPolicy,
	}
	recipients := []oortx.RecipientOutput{recAsset, recChange}

	makePkg := func(t *testing.T) *PreparedSubmitPackage {
		t.Helper()

		arkPSBT, err := oortx.BuildArkPSBT(
			[]oortx.CheckpointOutput{assetCPOut, floatCPOut},
			recipients,
		)
		require.NoError(t, err)

		return &PreparedSubmitPackage{
			ArkPSBT: arkPSBT,
			CheckpointPSBTs: []*psbt.Packet{
				assetCP,
				floatCPArt.PSBT,
			},
			CheckpointAssetRoots: []*chainhash.Hash{
				&cpRoot,
				nil,
			},
			TaprootAssetTransfer: &oortx.TaprootAssetTransfer{
				Version: oortx.TaprootAssetTransferVersion,
				CheckpointPackages: [][]byte{
					[]byte("cp-0"),
					nil,
				},
				ArkPackage: []byte("ark"),
			},
		}
	}

	inputs := []TransferInput{assetIn, floatIn}

	// Swapped checkpoints fail AttachTapLeafScripts and Validate.
	swappedCP := makePkg(t)
	swappedCP.CheckpointPSBTs = []*psbt.Packet{floatCPArt.PSBT, assetCP}
	require.ErrorContains(
		t, swappedCP.AttachTapLeafScripts(inputs),
		"input mismatch",
	)
	require.ErrorContains(
		t, swappedCP.Validate(cpPolicy, inputs, recipients),
		"input mismatch",
	)

	// Swapped asset checkpoint package slots fail Validate.
	swappedSlots := makePkg(t)
	require.NoError(t, swappedSlots.AttachTapLeafScripts(inputs))
	swappedSlots.TaprootAssetTransfer.CheckpointPackages = [][]byte{
		nil,
		[]byte("cp-1"),
	}
	require.ErrorContains(
		t, swappedSlots.Validate(cpPolicy, inputs, recipients),
		"checkpoint package presence mismatch",
	)

	// Duplicate input outpoints fail both AttachTapLeafScripts and
	// Validate.
	dupInputs := []TransferInput{assetIn, assetIn}
	dupPkg := makePkg(t)
	require.ErrorContains(
		t, dupPkg.AttachTapLeafScripts(dupInputs),
		"duplicate prepared input outpoint",
	)
	require.ErrorContains(
		t, dupPkg.Validate(cpPolicy, dupInputs, recipients),
		"duplicate prepared input outpoint",
	)
}
