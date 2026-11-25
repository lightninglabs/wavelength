package oor

import (
	"bytes"
	"context"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	clientdb "github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/ledger"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	oortx "github.com/lightninglabs/wavelength/lib/tx/oor"
	"github.com/lightninglabs/wavelength/lib/tx/psbtutil"
	"github.com/lightninglabs/wavelength/rpc/oorpb"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// newTestComposedAssetInput constructs a standard asset-bearing VTXO input
// whose pkScript commits to its VTXO policy composed with assetRoot.
func newTestComposedAssetInput(t *testing.T, clientKey *btcec.PrivateKey,
	operatorKey *btcec.PublicKey, outpoint wire.OutPoint,
	carrier btcutil.Amount, assetRoot chainhash.Hash, assetRef string,
	assetAmount uint64, origin oortx.CarrierOrigin) TransferInput {

	t.Helper()

	const exitDelay = uint32(144)
	policyBytes, err := arkscript.EncodeStandardVTXOTemplate(
		clientKey.PubKey(), operatorKey, exitDelay,
	)
	require.NoError(t, err)

	template, err := arkscript.DecodePolicyTemplate(policyBytes)
	require.NoError(t, err)
	compiled, err := template.Compile()
	require.NoError(t, err)
	composed, err := arkscript.ComposeWithSiblingRoot(compiled, assetRoot)
	require.NoError(t, err)
	pkScript, err := txscript.PayToTaprootScript(composed.OutputKey())
	require.NoError(t, err)

	tapScript, err := arkscript.VTXOTapScript(
		clientKey.PubKey(), operatorKey, exitDelay,
	)
	require.NoError(t, err)

	root := assetRoot

	return TransferInput{
		VTXO: &vtxo.Descriptor{
			Outpoint:       outpoint,
			Amount:         carrier,
			PkScript:       pkScript,
			PolicyTemplate: policyBytes,
			ClientKey: keychain.KeyDescriptor{
				KeyLocator: keychain.KeyLocator{
					Family: 1,
					Index:  7,
				},
				PubKey: clientKey.PubKey(),
			},
			OperatorKey:        operatorKey,
			TapScript:          tapScript,
			RelativeExpiry:     exitDelay,
			Status:             vtxo.VTXOStatusLive,
			TaprootAssetRoot:   &root,
			TaprootAssetRef:    assetRef,
			TaprootAssetAmount: assetAmount,
		},
		VTXOPolicyTemplate:        policyBytes,
		TaprootAssetCarrierOrigin: origin,
	}
}

// newTestComposedAssetRecipient builds an Ark recipient output whose pkScript
// commits to its standard VTXO policy composed with assetRoot.
func newTestComposedAssetRecipient(t *testing.T,
	owner, operator *btcec.PublicKey, carrier btcutil.Amount,
	assetRoot chainhash.Hash, assetRef string,
	assetAmount uint64) oortx.RecipientOutput {

	t.Helper()

	policyBytes, err := arkscript.EncodeStandardVTXOTemplate(
		owner, operator, 144,
	)
	require.NoError(t, err)
	template, err := arkscript.DecodePolicyTemplate(policyBytes)
	require.NoError(t, err)
	compiled, err := template.Compile()
	require.NoError(t, err)
	composed, err := arkscript.ComposeWithSiblingRoot(compiled, assetRoot)
	require.NoError(t, err)
	pkScript, err := txscript.PayToTaprootScript(composed.OutputKey())
	require.NoError(t, err)

	return oortx.RecipientOutput{
		PkScript:           pkScript,
		Value:              carrier,
		VTXOPolicyTemplate: policyBytes,
		Asset: &oortx.RecipientAsset{
			Root:   assetRoot,
			Ref:    assetRef,
			Amount: assetAmount,
		},
	}
}

// mustSerializePSBTForAssetTest serializes a PSBT for asset test fixtures.
func mustSerializePSBTForAssetTest(t *testing.T, pkt *psbt.Packet) []byte {
	t.Helper()

	raw, err := psbtutil.Serialize(pkt)
	require.NoError(t, err)

	return raw
}

// signCheckpointLegForTest attaches one tapscript signature on checkpoint
// input 0 for signerKey using in.EffectiveSpendPath().
func signCheckpointLegForTest(t *testing.T, signerKey *btcec.PrivateKey,
	in *TransferInput, checkpoint *psbt.Packet) {

	t.Helper()

	spendPath, err := in.EffectiveSpendPath()
	require.NoError(t, err)

	prevOut := &wire.TxOut{
		Value:    int64(in.VTXO.Amount),
		PkScript: in.VTXO.PkScript,
	}
	prevFetcher := txscript.NewCannedPrevOutputFetcher(
		prevOut.PkScript, prevOut.Value,
	)
	sigHashes := txscript.NewTxSigHashes(checkpoint.UnsignedTx, prevFetcher)
	signDesc := spendPath.SpendInfo.BuildSignDescriptor(
		keychain.KeyDescriptor{
			PubKey: signerKey.PubKey(),
		},
		prevOut,
		sigHashes,
		prevFetcher,
		0,
	)

	mockSigner := input.NewMockSigner([]*btcec.PrivateKey{signerKey}, nil)
	sig, err := mockSigner.SignOutputRaw(checkpoint.UnsignedTx, signDesc)
	require.NoError(t, err)

	require.NoError(
		t, psbtutil.AddTapLeafScript(
			&checkpoint.Inputs[0], spendPath.SpendInfo,
		),
	)
	require.NoError(
		t,
		psbtutil.AddTaprootScriptSpendSig(
			&checkpoint.Inputs[0], signerKey.PubKey(),
			spendPath.WitnessScript, sig.Serialize(),
			signDesc.HashType,
		),
	)
}

// signArkInputLegForTest attaches one tapscript signature on arkPSBT input idx
// for signerKey using the TaprootLeafScript already present on that input.
func signArkInputLegForTest(t *testing.T, signerKey *btcec.PrivateKey,
	arkPSBT *psbt.Packet, idx int) {

	t.Helper()

	prevOuts := make(
		map[wire.OutPoint]*wire.TxOut, len(arkPSBT.UnsignedTx.TxIn),
	)
	for i, txIn := range arkPSBT.UnsignedTx.TxIn {
		prevOuts[txIn.PreviousOutPoint] = arkPSBT.Inputs[i].WitnessUtxo
	}
	prevFetcher := txscript.NewMultiPrevOutFetcher(prevOuts)
	sigHashes := txscript.NewTxSigHashes(arkPSBT.UnsignedTx, prevFetcher)

	leaf := arkPSBT.Inputs[idx].TaprootLeafScript[0]
	signDesc := &input.SignDescriptor{
		KeyDesc: keychain.KeyDescriptor{
			PubKey: signerKey.PubKey(),
		},
		SignMethod:        input.TaprootScriptSpendSignMethod,
		Output:            arkPSBT.Inputs[idx].WitnessUtxo,
		HashType:          txscript.SigHashDefault,
		SigHashes:         sigHashes,
		PrevOutputFetcher: prevFetcher,
		InputIndex:        idx,
		WitnessScript:     leaf.Script,
		ControlBlock:      leaf.ControlBlock,
	}

	mockSigner := input.NewMockSigner([]*btcec.PrivateKey{signerKey}, nil)
	sig, err := mockSigner.SignOutputRaw(arkPSBT.UnsignedTx, signDesc)
	require.NoError(t, err)

	require.NoError(
		t,
		psbtutil.AddTaprootScriptSpendSig(
			&arkPSBT.Inputs[idx], signerKey.PubKey(), leaf.Script,
			sig.Serialize(), signDesc.HashType,
		),
	)
}

// TestOperatorFundedInputValidate pins the operator-funded float input
// contract: no local client key, no asset metadata, valid policy matching
// pkScript, and strict float/operator key separation.
func TestOperatorFundedInputValidate(t *testing.T) {
	t.Parallel()

	floatKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	policyBytes, pkScript, err := arkscript.EncodeStandardVTXOArtifacts(
		floatKey.PubKey(), operatorKey.PubKey(), 144,
	)
	require.NoError(t, err)
	lease := &OORCarrierLease{
		Outpoint: wire.OutPoint{
			Hash: [32]byte{
				0x0f,
			},
			Index: 0,
		},
		Value:          25_000,
		PolicyTemplate: policyBytes,
		PkScript:       pkScript,
		ExpiresAtUnix:  1_800_000_000,
	}

	valid, err := lease.ToTransferInput(
		operatorKey.PubKey(), floatKey.PubKey(),
	)
	require.NoError(t, err)
	require.NoError(t, valid.Validate())

	root := chainhash.Hash{0xaa}
	tests := []struct {
		name    string
		mutate  func(*TransferInput)
		wantErr string
	}{
		{
			name: "client key rejected",
			mutate: func(in *TransferInput) {
				in.VTXO.ClientKey.PubKey = floatKey.PubKey()
			},
			wantErr: "cannot carry a client key",
		},
		{
			name: "missing policy template",
			mutate: func(in *TransferInput) {
				in.VTXOPolicyTemplate = nil
			},
			wantErr: "requires a policy template",
		},
		{
			name: "missing owner leaf policy",
			mutate: func(in *TransferInput) {
				in.OwnerLeafPolicy = nil
			},
			wantErr: "requires an owner leaf policy",
		},
		{
			name: "missing operator key",
			mutate: func(in *TransferInput) {
				in.VTXO.OperatorKey = nil
			},
			wantErr: "requires an operator key",
		},
		{
			name: "custom spend rejected",
			mutate: func(in *TransferInput) {
				in.CustomSpend = &arkscript.SpendPath{}
			},
			wantErr: "cannot use a custom spend path",
		},
		{
			name: "asset metadata rejected",
			mutate: func(in *TransferInput) {
				in.VTXO.TaprootAssetRoot = &root
				in.VTXO.TaprootAssetRef = "asset"
				in.VTXO.TaprootAssetAmount = 10
			},
			wantErr: "cannot carry a taproot asset",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			in, err := lease.ToTransferInput(
				operatorKey.PubKey(), floatKey.PubKey(),
			)
			require.NoError(t, err)
			tc.mutate(&in)
			require.ErrorContains(t, in.Validate(), tc.wantErr)
		})
	}
}

// recordingBindingStore captures package bindings written by
// persistOutgoingPackage so tests can assert exact outpoints and input indices.
type recordingBindingStore struct {
	bindings []recordedBinding
}

type recordedBinding struct {
	outpoint   wire.OutPoint
	inputIndex uint32
	kind       PackageLinkKind
}

func (s *recordingBindingStore) UpsertPackage(_ context.Context,
	_ PackageDirection, _ chainhash.Hash, _ *psbt.Packet,
	_ []*psbt.Packet) error {

	return nil
}

func (s *recordingBindingStore) UpsertBinding(_ context.Context,
	outpoint wire.OutPoint, _ chainhash.Hash, inputIndex uint32,
	kind PackageLinkKind) error {

	s.bindings = append(s.bindings, recordedBinding{
		outpoint:   outpoint,
		inputIndex: inputIndex,
		kind:       kind,
	})

	return nil
}

// TestComposedAssetAndFloatOORSigningEndToEnd exercises a full composed
// Taproot Asset OOR transfer backed by an operator carrier float in both input
// orders (asset first and float first), verifying carrier planning, composed
// control blocks, wire round-trips, all-phase v7 snapshot persistence, float
// exclusion from ledger/summary/bindings/spend-marking, and full Bitcoin
// script-VM execution.
func TestComposedAssetAndFloatOORSigningEndToEnd(t *testing.T) {
	t.Parallel()

	t.Run("asset first", func(t *testing.T) {
		t.Parallel()
		runComposedAssetAndFloatEndToEnd(t, false)
	})
	t.Run("float first", func(t *testing.T) {
		t.Parallel()
		runComposedAssetAndFloatEndToEnd(t, true)
	})
}

func runComposedAssetAndFloatEndToEnd(t *testing.T, floatFirst bool) {
	t.Helper()

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	receiverKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	floatKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	const (
		assetRef        = "group:01020304"
		outputFloor     = btcutil.Amount(1_000)
		boardedCarrier  = btcutil.Amount(2_500)
		floatLeaseValue = btcutil.Amount(10_000)
	)

	// Step 1: Plan carrier allocation for a partial send (2 new asset
	// leaves) spending one round-created asset leaf and one leased float.
	plan, err := oortx.PlanAssetCarriers(oortx.CarrierPlanRequest{
		Inputs: []oortx.CarrierInput{{
			Amount: boardedCarrier,
			Origin: oortx.CarrierOriginRound,
		}},
		NewAssetLeafCount: 2,
		OutputFloor:       outputFloor,
		LeaseValue:        floatLeaseValue,
	})
	require.NoError(t, err)
	require.Equal(t, btcutil.Amount(2_500), plan.SenderChange)
	require.Equal(t, btcutil.Amount(8_000), plan.OperatorChange)

	// Step 2: Build the composed asset input and the leased float input.
	inputAssetRoot := chainhash.Hash{0x11, 0x22}
	assetInput := newTestComposedAssetInput(
		t, clientKey, operatorKey.PubKey(), wire.OutPoint{
			Hash:  [32]byte{0x01},
			Index: 0,
		},
		boardedCarrier,
		inputAssetRoot,
		assetRef,
		100,
		oortx.CarrierOriginRound,
	)

	floatPolicyBytes, floatPkScript, err := arkscript.
		EncodeStandardVTXOArtifacts(
			floatKey.PubKey(), operatorKey.PubKey(), 144,
		)
	require.NoError(t, err)
	lease := &OORCarrierLease{
		Outpoint: wire.OutPoint{
			Hash: [32]byte{
				0x02,
			},
			Index: 0,
		},
		Value:          floatLeaseValue,
		PolicyTemplate: floatPolicyBytes,
		PkScript:       floatPkScript,
		ExpiresAtUnix:  1_800_000_000,
	}
	floatInput, err := lease.ToTransferInput(
		operatorKey.PubKey(), floatKey.PubKey(),
	)
	require.NoError(t, err)

	require.NoError(t, assetInput.Validate())
	require.NoError(t, floatInput.Validate())

	// Step 3: Build checkpoint PSBTs — compose the asset checkpoint output
	// with checkpointAssetRoot while leaving the float checkpoint plain.
	cpPolicy := arkscript.CheckpointPolicy{
		OperatorKey: operatorKey.PubKey(),
		CSVDelay:    10,
	}

	assetCPIn, err := assetInput.CheckpointInput()
	require.NoError(t, err)
	assetCPArt, err := oortx.BuildCheckpointPSBT(cpPolicy, assetCPIn)
	require.NoError(t, err)

	checkpointAssetRoot := chainhash.Hash{0x33, 0x44}
	composedCPPkScript, err := expectedCheckpointPkScript(
		cpPolicy, assetCPArt.OwnerLeafScript, &checkpointAssetRoot,
	)
	require.NoError(t, err)

	composedCPTx := assetCPArt.PSBT.UnsignedTx.Copy()
	composedCPTx.TxOut[0].PkScript = composedCPPkScript
	assetCPPSBT, err := psbt.NewFromUnsignedTx(composedCPTx)
	require.NoError(t, err)
	assetCPPSBT.Inputs[0].WitnessUtxo =
		assetCPArt.PSBT.Inputs[0].WitnessUtxo
	assetCPPSBT.Outputs[0].TaprootTapTree = assetCPArt.TapTreeEncoded
	assetCPArt.PSBT = assetCPPSBT

	floatCPIn, err := floatInput.CheckpointInput()
	require.NoError(t, err)
	floatCPArt, err := oortx.BuildCheckpointPSBT(cpPolicy, floatCPIn)
	require.NoError(t, err)

	assetCPOut, err := assetCPArt.ToCheckpointOutput()
	require.NoError(t, err)
	floatCPOut, err := floatCPArt.ToCheckpointOutput()
	require.NoError(t, err)

	// Arrange inputs, checkpoints, roots, and slots according to input
	// ordering.
	var (
		inputs          []TransferInput
		checkpointPSBTs []*psbt.Packet
		cpOutputs       []oortx.CheckpointOutput
		cpRoots         []*chainhash.Hash
		cpPackages      [][]byte
		assetIdx        int
		floatIdx        int
	)
	if floatFirst {
		assetIdx = 1
		floatIdx = 0
		inputs = []TransferInput{floatInput, assetInput}
		checkpointPSBTs = []*psbt.Packet{floatCPArt.PSBT, assetCPPSBT}
		cpOutputs = []oortx.CheckpointOutput{floatCPOut, assetCPOut}
		cpRoots = []*chainhash.Hash{nil, &checkpointAssetRoot}
		cpPackages = [][]byte{
			nil,
			[]byte("sealed-checkpoint-asset-package"),
		}
	} else {
		assetIdx = 0
		floatIdx = 1
		inputs = []TransferInput{assetInput, floatInput}
		checkpointPSBTs = []*psbt.Packet{assetCPPSBT, floatCPArt.PSBT}
		cpOutputs = []oortx.CheckpointOutput{assetCPOut, floatCPOut}
		cpRoots = []*chainhash.Hash{&checkpointAssetRoot, nil}
		cpPackages = [][]byte{
			[]byte("sealed-checkpoint-asset-package"),
			nil,
		}
	}

	// Step 4: Build Ark recipient outputs matching the carrier plan.
	senderChangePolicy, senderChangePkScript, err := arkscript.
		EncodeStandardVTXOArtifacts(
			clientKey.PubKey(), operatorKey.PubKey(), 144,
		)
	require.NoError(t, err)

	recipients := []oortx.RecipientOutput{
		newTestComposedAssetRecipient(
			t, receiverKey.PubKey(), operatorKey.PubKey(),
			plan.NewAssetLeafCarrier, chainhash.Hash{0x55, 0x66},
			assetRef, 70,
		),
		newTestComposedAssetRecipient(
			t, clientKey.PubKey(), operatorKey.PubKey(),
			plan.NewAssetLeafCarrier, chainhash.Hash{0x77, 0x88},
			assetRef, 30,
		),
		{
			PkScript:           senderChangePkScript,
			Value:              plan.SenderChange,
			VTXOPolicyTemplate: senderChangePolicy,
		},
		{
			PkScript:           lease.PkScript,
			Value:              plan.OperatorChange,
			VTXOPolicyTemplate: lease.PolicyTemplate,
		},
	}

	arkPSBT, err := oortx.BuildArkPSBT(cpOutputs, recipients)
	require.NoError(t, err)

	assetTransfer := &oortx.TaprootAssetTransfer{
		Version:            oortx.TaprootAssetTransferVersion,
		CheckpointPackages: cpPackages,
		ArkPackage:         []byte("sealed-ark-asset-package"),
	}
	prepared := &PreparedSubmitPackage{
		ArkPSBT:              arkPSBT,
		CheckpointPSBTs:      checkpointPSBTs,
		CheckpointAssetRoots: cpRoots,
		TaprootAssetTransfer: assetTransfer,
	}

	roundTripPhase := func(sid SessionID, st State) (State, []byte) {
		t.Helper()

		snap, err := NewOutgoingSnapshot(sid, st)
		require.NoError(t, err)
		require.Equal(t, outgoingSnapshotVersion, snap.Version)

		raw, err := encodeOutgoingSnapshot(snap)
		require.NoError(t, err)
		dec, err := decodeOutgoingSnapshot(raw)
		require.NoError(t, err)
		restored, err := OutgoingStateFromSnapshot(dec)
		require.NoError(t, err)
		require.Equal(
			t, assetTransfer, stateTaprootAssetTransfer(restored),
		)

		return restored, raw
	}

	// Step 5: Drive Idle -> AwaitingArkSignatures via StartTransferEvent.
	ctx := context.Background()
	transition, err := (&Idle{}).ProcessEvent(ctx, &StartTransferEvent{
		VTXOInputs:       inputs,
		RecipientOutputs: recipients,
		Policy:           cpPolicy,
		PreparedSubmit:   prepared,
	}, nil)
	require.NoError(t, err)

	awaitArk, ok := transition.NextState.(*AwaitingArkSignatures)
	require.True(t, ok)
	sessionID := SessionID(awaitArk.ArkPSBT.UnsignedTx.TxHash())

	restoredArkState, _ := roundTripPhase(sessionID, awaitArk)
	awaitArk, ok = restoredArkState.(*AwaitingArkSignatures)
	require.True(t, ok)

	// Client signs Ark PSBT using ONLY clientKey (cannot sign floatKey).
	clientSigner := input.NewMockSigner(
		[]*btcec.PrivateKey{clientKey}, nil,
	)
	require.NoError(
		t, SignArkPSBT(
			clientSigner, awaitArk.ArkPSBT,
			awaitArk.CheckpointPSBTs, awaitArk.TransferInputs,
		),
	)

	// Step 6: Drive AwaitingArkSignatures -> AwaitingSubmitAccepted.
	transition, err = awaitArk.ProcessEvent(ctx, &ArkSignedEvent{
		ArkPSBT: awaitArk.ArkPSBT,
	}, nil)
	require.NoError(t, err)

	awaitSubmit, ok := transition.NextState.(*AwaitingSubmitAccepted)
	require.True(t, ok)

	emitted := transition.NewEvents.UnwrapOr(EmittedEvent{})
	submitReq, ok := emitted.Outbox[0].(*SendSubmitPackageRequest)
	require.True(t, ok)
	require.Equal(t, assetTransfer, submitReq.TaprootAssetTransfer)

	// Verify wire marshaling through oorpb carries all asset descriptors,
	// asset recipients, and TaprootAssetTransfer, and legacy parser fails
	// closed.
	protoRes := submitReq.ToProto()
	require.True(t, protoRes.IsOk())
	protoReq, ok := protoRes.UnwrapOr(nil).(*oorpb.SubmitPackageRequest)
	require.True(t, ok)

	_, _, _, _, err = oorpb.ParseSubmitPackageRequest(protoReq)
	require.ErrorContains(t, err, "carries taproot asset transfer")

	_, _, gotDescs, gotRecipients, gotTransfer, err :=
		oorpb.ParseSubmitPackageRequestWithAssets(protoReq)
	require.NoError(t, err)
	require.Len(t, gotDescs, 2)
	require.NotNil(t, gotDescs[assetIdx].TaprootAssetRoot)
	require.Equal(t, inputAssetRoot, *gotDescs[assetIdx].TaprootAssetRoot)
	require.Equal(t, assetRef, gotDescs[assetIdx].TaprootAssetRef)
	require.Equal(t, uint64(100), gotDescs[assetIdx].TaprootAssetAmount)
	require.Nil(t, gotDescs[floatIdx].TaprootAssetRoot)
	require.Len(t, gotRecipients, 4)
	require.Equal(t, assetTransfer, gotTransfer)

	// Verify OutgoingSnapshot v7 TLV round-trip and FillOutgoingSummary
	// excluding the operator float outpoint and amount.
	restoredState, snapRaw := roundTripPhase(sessionID, awaitSubmit)
	restoredSubmit, ok := restoredState.(*AwaitingSubmitAccepted)
	require.True(t, ok)
	require.Len(t, restoredSubmit.RecipientOutputs, 4)
	require.Len(t, restoredSubmit.TransferInputs, 2)
	require.False(t, restoredSubmit.TransferInputs[assetIdx].OperatorFunded)
	require.Equal(
		t, oortx.CarrierOriginRound,
		restoredSubmit.TransferInputs[assetIdx].
			TaprootAssetCarrierOrigin,
	)
	require.True(t, restoredSubmit.TransferInputs[floatIdx].OperatorFunded)
	require.Nil(
		t,
		restoredSubmit.TransferInputs[floatIdx].VTXO.ClientKey.PubKey,
	)

	var summary SessionSummary
	summaryRec := &clientdb.OORSessionRegistryRecord{
		Direction:    clientdb.OORSessionDirectionOutgoing,
		SnapshotData: snapRaw,
	}
	require.NoError(t, FillOutgoingSummary(&summary, summaryRec))
	require.Equal(
		t, []wire.OutPoint{assetInput.VTXO.Outpoint},
		summary.InputOutpoints,
	)
	require.Equal(t, int64(boardedCarrier), summary.InputAmountSat)

	// Step 7: Operator cosigns Ark PSBT and checkpoint PSBTs.
	coSignedArk, err := psbtutil.Parse(
		mustSerializePSBTForAssetTest(t, awaitSubmit.ArkPSBT),
	)
	require.NoError(t, err)
	assetCPTxid := assetCPPSBT.UnsignedTx.TxHash()
	floatCPTxid := floatCPArt.PSBT.UnsignedTx.TxHash()
	for i, txIn := range coSignedArk.UnsignedTx.TxIn {
		switch txIn.PreviousOutPoint.Hash {
		case assetCPTxid:
			signArkInputLegForTest(t, operatorKey, coSignedArk, i)

		case floatCPTxid:
			signArkInputLegForTest(t, operatorKey, coSignedArk, i)
			signArkInputLegForTest(t, floatKey, coSignedArk, i)

		default:
			t.Fatalf("unexpected checkpoint input %s",
				txIn.PreviousOutPoint.Hash)
		}
	}

	coSignedAssetCP, err := psbtutil.Parse(
		mustSerializePSBTForAssetTest(t, assetCPPSBT),
	)
	require.NoError(t, err)
	coSignedFloatCP, err := psbtutil.Parse(
		mustSerializePSBTForAssetTest(t, floatCPArt.PSBT),
	)
	require.NoError(t, err)

	signCheckpointLegForTest(
		t, operatorKey, &inputs[assetIdx], coSignedAssetCP,
	)
	signCheckpointLegForTest(
		t, operatorKey, &inputs[floatIdx], coSignedFloatCP,
	)

	makeCPs := func(assetCP, floatCP *psbt.Packet) []*psbt.Packet {
		if floatFirst {
			return []*psbt.Packet{floatCP, assetCP}
		}

		return []*psbt.Packet{assetCP, floatCP}
	}

	// Missing float-owner leg on the operator-funded checkpoint must be
	// rejected before the client signs its own asset VTXO.
	err = SignCheckpointPSBTs(
		clientSigner, restoredSubmit.TransferInputs,
		makeCPs(coSignedAssetCP, coSignedFloatCP),
	)
	require.ErrorContains(t, err, "float owner signature")

	// Non-default sighash on the float-owner leg is rejected.
	badSighashFloatCP, err := psbtutil.Parse(
		mustSerializePSBTForAssetTest(t, coSignedFloatCP),
	)
	require.NoError(t, err)
	signCheckpointLegForTest(
		t, floatKey, &inputs[floatIdx], badSighashFloatCP,
	)
	for _, s := range badSighashFloatCP.Inputs[0].TaprootScriptSpendSig {
		if bytes.Equal(
			s.XOnlyPubKey,
			floatKey.PubKey().SerializeCompressed()[1:],
		) {

			s.SigHash = txscript.SigHashNone |
				txscript.SigHashAnyOneCanPay
		}
	}
	err = SignCheckpointPSBTs(
		clientSigner, restoredSubmit.TransferInputs,
		makeCPs(coSignedAssetCP, badSighashFloatCP),
	)
	require.ErrorContains(t, err, "SIGHASH_DEFAULT")

	// Corrupted float-owner signature is rejected before client signing.
	badSigFloatCP, err := psbtutil.Parse(
		mustSerializePSBTForAssetTest(t, coSignedFloatCP),
	)
	require.NoError(t, err)
	signCheckpointLegForTest(t, floatKey, &inputs[floatIdx], badSigFloatCP)
	for _, s := range badSigFloatCP.Inputs[0].TaprootScriptSpendSig {
		if bytes.Equal(
			s.XOnlyPubKey,
			floatKey.PubKey().SerializeCompressed()[1:],
		) {

			s.Signature[0] ^= 0xff
		}
	}
	err = SignCheckpointPSBTs(
		clientSigner, restoredSubmit.TransferInputs,
		makeCPs(coSignedAssetCP, badSigFloatCP),
	)
	require.ErrorContains(t, err, "verify float owner signature")

	// Attach the valid float-owner leg and drive SubmitAccepted ->
	// CoSigned.
	signCheckpointLegForTest(
		t, floatKey, &inputs[floatIdx], coSignedFloatCP,
	)

	// Duplicate input outpoint aliasing is rejected by SignCheckpointPSBTs.
	aliasedInputs := []TransferInput{
		restoredSubmit.TransferInputs[assetIdx],
		restoredSubmit.TransferInputs[assetIdx],
	}
	err = SignCheckpointPSBTs(
		clientSigner, aliasedInputs,
		[]*psbt.Packet{coSignedAssetCP, coSignedAssetCP},
	)
	require.ErrorContains(t, err, "duplicate transfer input outpoint")

	// Mismatched checkpoint outpoint is rejected by signCheckpointPSBT.
	err = signCheckpointPSBT(
		clientSigner, &restoredSubmit.TransferInputs[assetIdx],
		coSignedFloatCP,
	)
	require.ErrorContains(t, err, "does not match transfer input")

	transition, err = restoredSubmit.ProcessEvent(ctx, &SubmitAcceptedEvent{
		SessionID: sessionID,
		ArkPSBT:   coSignedArk,
		CoSignedCheckpointPSBTs: makeCPs(
			coSignedAssetCP, coSignedFloatCP,
		),
	}, nil)
	require.NoError(t, err)

	awaitCP, ok := transition.NextState.(*AwaitingCheckpointSignatures)
	require.True(t, ok)

	restoredCPState, _ := roundTripPhase(sessionID, awaitCP)
	awaitCP, ok = restoredCPState.(*AwaitingCheckpointSignatures)
	require.True(t, ok)

	// Step 8: Client signs its composed asset checkpoint and skips float.
	require.NoError(
		t, SignCheckpointPSBTs(
			clientSigner, awaitCP.TransferInputs,
			awaitCP.CoSignedCheckpointPSBTs,
		),
	)

	// Step 9: Drive CheckpointsSigned (runs ValidateFinalizePackageSigned
	// executing the real Bitcoin script VM on BOTH checkpoints!) ->
	// FinalizeAccepted -> AwaitingLocalVTXOUpdate.
	transition, err = awaitCP.ProcessEvent(ctx, &CheckpointsSignedEvent{
		FinalCheckpointPSBTs: awaitCP.CoSignedCheckpointPSBTs,
	}, nil)
	require.NoError(t, err)

	awaitFinalize, ok := transition.NextState.(*AwaitingFinalizeAccepted)
	require.True(t, ok)

	restoredFinalizeState, _ := roundTripPhase(sessionID, awaitFinalize)
	awaitFinalize, ok = restoredFinalizeState.(*AwaitingFinalizeAccepted)
	require.True(t, ok)

	// Verify session-actor ledger accounting and package bindings exclude
	// the operator float and preserve the wallet input's original index.
	pkgStore := &recordingBindingStore{}
	behavior := &sessionBehavior{
		cfg: SessionActorConfig{
			PackageStore: pkgStore,
			LedgerSink: fn.Some[ledger.Sink](
				&recordingLedgerSink{},
			),
		},
		sessionID: sessionID,
		log:       btclog.Disabled,
	}
	require.NoError(
		t, behavior.persistOutgoingPackage(ctx, awaitFinalize),
	)
	require.Equal(t, []recordedBinding{{
		outpoint:   assetInput.VTXO.Outpoint,
		inputIndex: uint32(assetIdx),
		kind:       PackageLinkKindConsumedInput,
	}}, pkgStore.bindings)

	behavior.queueVTXOSent(ctx, awaitFinalize)
	require.Len(t, behavior.pendingLedger, 1)
	sentMsg, ok := behavior.pendingLedger[0].(*ledger.VTXOSentMsg)
	require.True(t, ok)
	require.Equal(t, int64(boardedCarrier), sentMsg.AmountSat)

	transition, err = awaitFinalize.ProcessEvent(
		ctx, &FinalizeAcceptedEvent{}, nil,
	)
	require.NoError(t, err)

	awaitLocal, ok := transition.NextState.(*AwaitingLocalVTXOUpdate)
	require.True(t, ok)
	_, _ = roundTripPhase(sessionID, awaitLocal)

	finalEmitted := transition.NewEvents.UnwrapOr(EmittedEvent{})
	markSpent, ok := finalEmitted.Outbox[0].(*MarkInputsSpentRequest)
	require.True(t, ok)

	// Only the wallet's asset VTXO is marked spent; the operator float
	// outpoint must be excluded.
	require.Equal(
		t, []wire.OutPoint{assetInput.VTXO.Outpoint},
		markSpent.Outpoints,
	)
}

// TestPrePONRFailureExcludesOperatorFundedFromRelease verifies a pre-PONR
// failure releases only wallet-owned inputs and never hands an operator float
// outpoint to the local VTXO manager.
func TestPrePONRFailureExcludesOperatorFundedFromRelease(t *testing.T) {
	t.Parallel()

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	floatKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	assetInput := newTestComposedAssetInput(
		t, clientKey, operatorKey.PubKey(), wire.OutPoint{
			Hash:  [32]byte{0x0a},
			Index: 0,
		},
		2_000,
		chainhash.Hash{0xaa}, "asset-ref",
		50,
		oortx.CarrierOriginRound,
	)
	assetInput.ReserveEpoch = 9

	floatPolicy, floatScript, err := arkscript.EncodeStandardVTXOArtifacts(
		floatKey.PubKey(), operatorKey.PubKey(), 144,
	)
	require.NoError(t, err)
	lease := &OORCarrierLease{
		Outpoint: wire.OutPoint{
			Hash: [32]byte{
				0x0b,
			},
			Index: 0,
		},
		Value:          10_000,
		PolicyTemplate: floatPolicy,
		PkScript:       floatScript,
		ExpiresAtUnix:  1_800_000_000,
	}
	floatInput, err := lease.ToTransferInput(
		operatorKey.PubKey(), floatKey.PubKey(),
	)
	require.NoError(t, err)

	state := &AwaitingSubmitAccepted{
		TransferInputs: []TransferInput{
			assetInput,
			floatInput,
		},
	}

	transition, err := handleOutboxError(nil, state, &OutboxErrorEvent{
		OutboxType:  "submit",
		Retryable:   false,
		ErrorReason: "rejected",
	})
	require.NoError(t, err)

	emitted := transition.NewEvents.UnwrapOr(EmittedEvent{})
	require.Len(t, emitted.Outbox, 1)
	rel, ok := emitted.Outbox[0].(*ReleaseInputsRequest)
	require.True(t, ok)
	require.Equal(
		t, []wire.OutPoint{assetInput.VTXO.Outpoint}, rel.Outpoints,
	)
	require.Equal(
		t, map[wire.OutPoint]uint64{
			assetInput.VTXO.Outpoint: 9,
		},
		rel.ReserveEpochs,
	)
}

// TestPlainOORRejectsAssetAndFloatWithoutPreparedSubmit verifies that the
// plain Bitcoin OOR path fails closed if any asset input, operator-funded
// input, or asset recipient enters without a PreparedSubmitPackage.
func TestPlainOORRejectsAssetAndFloatWithoutPreparedSubmit(t *testing.T) {
	t.Parallel()

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	floatKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	receiverKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	cpPolicy := arkscript.CheckpointPolicy{
		OperatorKey: operatorKey.PubKey(),
		CSVDelay:    10,
	}

	assetInput := newTestComposedAssetInput(
		t, clientKey, operatorKey.PubKey(), wire.OutPoint{
			Hash:  [32]byte{0x11},
			Index: 0,
		},
		2_000, chainhash.Hash{0xaa}, "asset-ref", 50,
		oortx.CarrierOriginRound,
	)

	floatPolicy, floatScript, err := arkscript.EncodeStandardVTXOArtifacts(
		floatKey.PubKey(), operatorKey.PubKey(), 144,
	)
	require.NoError(t, err)
	floatLease := &OORCarrierLease{
		Outpoint: wire.OutPoint{
			Hash: [32]byte{
				0x12,
			},
			Index: 0,
		},
		Value:          10_000,
		PolicyTemplate: floatPolicy,
		PkScript:       floatScript,
		ExpiresAtUnix:  1_800_000_000,
	}
	floatInput, err := floatLease.ToTransferInput(
		operatorKey.PubKey(), floatKey.PubKey(),
	)
	require.NoError(t, err)

	plainRecipient := oortx.RecipientOutput{
		PkScript: floatScript,
		Value:    2_000,
	}
	assetRecipient := newTestComposedAssetRecipient(
		t, receiverKey.PubKey(), operatorKey.PubKey(), 1_000,
		chainhash.Hash{0xbb}, "asset-ref", 50,
	)

	ctx := context.Background()
	_, err = (&Idle{}).ProcessEvent(ctx, &StartTransferEvent{
		VTXOInputs:       []TransferInput{assetInput},
		RecipientOutputs: []oortx.RecipientOutput{plainRecipient},
		Policy:           cpPolicy,
	}, nil)
	require.ErrorContains(t, err, "requires a prepared submit package")

	_, err = (&Idle{}).ProcessEvent(ctx, &StartTransferEvent{
		VTXOInputs:       []TransferInput{floatInput},
		RecipientOutputs: []oortx.RecipientOutput{plainRecipient},
		Policy:           cpPolicy,
	}, nil)
	require.ErrorContains(t, err, "requires a prepared submit package")

	plainPolicy, plainScript, err := arkscript.EncodeStandardVTXOArtifacts(
		clientKey.PubKey(), operatorKey.PubKey(), 144,
	)
	require.NoError(t, err)
	plainTap, err := arkscript.VTXOTapScript(
		clientKey.PubKey(), operatorKey.PubKey(), 144,
	)
	require.NoError(t, err)
	plainInput := TransferInput{
		VTXO: &vtxo.Descriptor{
			Outpoint: wire.OutPoint{
				Hash: [32]byte{
					0x13,
				},
				Index: 0,
			},
			Amount:         1_000,
			PkScript:       plainScript,
			PolicyTemplate: plainPolicy,
			ClientKey: keychain.KeyDescriptor{
				PubKey: clientKey.PubKey(),
			},
			OperatorKey:    operatorKey.PubKey(),
			TapScript:      plainTap,
			RelativeExpiry: 144,
		},
		VTXOPolicyTemplate: plainPolicy,
	}
	_, err = (&Idle{}).ProcessEvent(ctx, &StartTransferEvent{
		VTXOInputs:       []TransferInput{plainInput},
		RecipientOutputs: []oortx.RecipientOutput{assetRecipient},
		Policy:           cpPolicy,
	}, nil)
	require.ErrorContains(t, err, "requires a prepared submit package")
}

// TestOutgoingSnapshotSizeLimitAndRestoreValidation verifies encode-time
// snapshot size bounding, future-version and missing-container rejection on
// restore, v6 backwards-compatible decode, and custom-spend restore.
func TestOutgoingSnapshotSizeLimitAndRestoreValidation(t *testing.T) {
	t.Parallel()

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	// Oversized snapshot fails at encode time so an unreadable blob is
	// never persisted.
	oversized := &OutgoingSnapshot{
		Version: outgoingSnapshotVersion,
		SessionID: SessionID{
			0x01,
		},
		Phase:                OutgoingPhaseSubmitSent,
		TaprootAssetTransfer: make([]byte, maxOORMessageSize+1),
	}
	_, err = encodeOutgoingSnapshot(oversized)
	require.ErrorContains(t, err, "exceeds max")

	// Future snapshot version is rejected on restore.
	_, err = OutgoingStateFromSnapshot(&OutgoingSnapshot{
		Version:   outgoingSnapshotVersion + 1,
		SessionID: SessionID{0x01},
		Phase:     OutgoingPhaseCompleted,
	})
	require.ErrorContains(t, err, "unsupported snapshot version")

	// Asset input restored without TaprootAssetTransfer fails closed.
	assetInput := newTestComposedAssetInput(
		t, clientKey, operatorKey.PubKey(), wire.OutPoint{
			Hash:  [32]byte{0x21},
			Index: 0,
		},
		2_000, chainhash.Hash{0xcc}, "asset-ref", 25,
		oortx.CarrierOriginRound,
	)
	assetSnap, err := assetInput.ToSnapshot()
	require.NoError(t, err)
	_, err = OutgoingStateFromSnapshot(&OutgoingSnapshot{
		Version:                outgoingSnapshotVersion,
		SessionID:              SessionID{0x02},
		Phase:                  OutgoingPhaseLocalVTXOUpdate,
		TransferInputSnapshots: []*TransferInputSnapshot{assetSnap},
	})
	require.ErrorContains(
		t, err, "requires taproot asset transfer container",
	)

	// Base v6 snapshot (no v7 recipient or asset records) decodes and
	// restores cleanly at head.
	plainPolicy, plainScript, err := arkscript.EncodeStandardVTXOArtifacts(
		clientKey.PubKey(), operatorKey.PubKey(), 144,
	)
	require.NoError(t, err)
	plainTap, err := arkscript.VTXOTapScript(
		clientKey.PubKey(), operatorKey.PubKey(), 144,
	)
	require.NoError(t, err)
	plainInput := TransferInput{
		VTXO: &vtxo.Descriptor{
			Outpoint: wire.OutPoint{
				Hash: [32]byte{
					0x22,
				},
				Index: 0,
			},
			Amount:         3_000,
			PkScript:       plainScript,
			PolicyTemplate: plainPolicy,
			ClientKey: keychain.KeyDescriptor{
				PubKey: clientKey.PubKey(),
			},
			OperatorKey:    operatorKey.PubKey(),
			TapScript:      plainTap,
			RelativeExpiry: 144,
		},
		VTXOPolicyTemplate: plainPolicy,
	}
	plainSnap, err := plainInput.ToSnapshot()
	require.NoError(t, err)

	v6Snap := &OutgoingSnapshot{
		Version: 6,
		SessionID: SessionID{
			0x03,
		},
		Phase: OutgoingPhaseLocalVTXOUpdate,
		TransferInputSnapshots: []*TransferInputSnapshot{
			plainSnap,
		},
	}
	v6Raw, err := encodeOutgoingSnapshot(v6Snap)
	require.NoError(t, err)
	v6Decoded, err := decodeOutgoingSnapshot(v6Raw)
	require.NoError(t, err)
	v6Restored, err := OutgoingStateFromSnapshot(v6Decoded)
	require.NoError(t, err)
	localUpdate, ok := v6Restored.(*AwaitingLocalVTXOUpdate)
	require.True(t, ok)
	require.Len(t, localUpdate.TransferInputs, 1)
	require.Equal(
		t, plainInput.VTXO.Outpoint,
		localUpdate.TransferInputs[0].VTXO.Outpoint,
	)

	// Custom-spend input survives TransferInputSnapshot round-trip with
	// Validate() enabled on restore.
	customSpendPath, err := plainInput.EffectiveSpendPath()
	require.NoError(t, err)
	customInput := plainInput
	customInput.CustomSpend = &arkscript.SpendPath{
		SpendInfo: customSpendPath.SpendInfo,
		Conditions: [][]byte{
			{
				0xde,
				0xad,
			},
		},
		RequiredSequence: 144,
	}
	customSnap, err := customInput.ToSnapshot()
	require.NoError(t, err)
	customRaw, err := encodeTransferInputSnapshot(customSnap)
	require.NoError(t, err)
	customDec, err := decodeTransferInputSnapshot(customRaw)
	require.NoError(t, err)
	customRestored, err := TransferInputFromSnapshot(customDec)
	require.NoError(t, err)
	require.NotNil(t, customRestored.CustomSpend)
	require.Equal(
		t, uint32(144), customRestored.CustomSpend.RequiredSequence,
	)
}
