package oor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/arkrpc"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	oortx "github.com/lightninglabs/wavelength/lib/tx/oor"
	"github.com/lightninglabs/wavelength/lib/tx/psbtutil"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// buildTestIncomingAssetGraph constructs a valid canonical Ark PSBT and
// checkpoint PSBT paying an asset-bearing composed output to recipientKey.
func buildTestIncomingAssetGraph(t *testing.T, recipientKey *btcec.PublicKey,
	operatorPriv *btcec.PrivateKey, exitDelay uint32,
	assetRoot chainhash.Hash, assetRef string, assetAmount uint64) (
	*psbt.Packet, []*psbt.Packet, []byte, []byte, []byte,
	*oortx.TaprootAssetTransfer) {

	t.Helper()

	operatorKey := operatorPriv.PubKey()
	senderPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	policy := arkscript.CheckpointPolicy{
		OperatorKey: operatorKey,
		CSVDelay:    exitDelay,
	}

	inVTXO := newTestComposedAssetInput(
		t, senderPriv, operatorKey, wire.OutPoint{
			Hash:  chainhash.Hash{0x11},
			Index: 0,
		},
		1_000,
		chainhash.Hash{0x22},
		assetRef,
		assetAmount,
		oortx.CarrierOriginRound,
	)

	collabLeaf, err := arkscript.MultiSigCollabTapLeaf(
		senderPriv.PubKey(), operatorKey,
	)
	require.NoError(t, err)

	cpArtifact, err := oortx.BuildCheckpointPSBT(
		policy, oortx.CheckpointInput{
			SpentVTXO: oortx.SpentVTXORef{
				Outpoint: inVTXO.VTXO.Outpoint,
				Output: wire.NewTxOut(
					int64(inVTXO.VTXO.Amount),
					inVTXO.VTXO.PkScript,
				),
			},
			OwnerLeafScript: collabLeaf.Script,
		},
	)
	require.NoError(t, err)
	cpOut, err := cpArtifact.ToCheckpointOutput()
	require.NoError(t, err)

	recipPolicyBytes, err := arkscript.EncodeStandardVTXOTemplate(
		recipientKey, operatorKey, exitDelay,
	)
	require.NoError(t, err)
	recipTemplate, err := arkscript.DecodePolicyTemplate(recipPolicyBytes)
	require.NoError(t, err)
	recipCompiled, err := recipTemplate.Compile()
	require.NoError(t, err)
	routingPkScript, err := txscript.PayToTaprootScript(
		recipCompiled.OutputKey(),
	)
	require.NoError(t, err)
	recipComposed, err := arkscript.ComposeWithSiblingRoot(
		recipCompiled, assetRoot,
	)
	require.NoError(t, err)
	composedPkScript, err := txscript.PayToTaprootScript(
		recipComposed.OutputKey(),
	)
	require.NoError(t, err)

	arkPSBT, err := oortx.BuildArkPSBT(
		[]oortx.CheckpointOutput{cpOut},
		[]oortx.RecipientOutput{{
			PkScript:           composedPkScript,
			Value:              1_000,
			VTXOPolicyTemplate: recipPolicyBytes,
			Asset: &oortx.RecipientAsset{
				Root:   assetRoot,
				Ref:    assetRef,
				Amount: assetAmount,
			},
		}},
	)
	require.NoError(t, err)

	signCheckpointLegForTest(t, senderPriv, &inVTXO, cpArtifact.PSBT)
	signCheckpointLegForTest(t, operatorPriv, &inVTXO, cpArtifact.PSBT)

	arkLeaf, err := oortx.BuildTaprootTapLeafScript(
		cpArtifact.PSBT.Outputs[0].TaprootTapTree, collabLeaf.Script,
	)
	require.NoError(t, err)
	addTaprootLeafScript(&arkPSBT.Inputs[0], arkLeaf)
	signArkInputLegForTest(t, senderPriv, arkPSBT, 0)
	signArkInputLegForTest(t, operatorPriv, arkPSBT, 0)

	xfer := &oortx.TaprootAssetTransfer{
		Version:    oortx.TaprootAssetTransferVersion,
		ArkPackage: []byte("sealed-ark-asset-package"),
		CheckpointPackages: [][]byte{
			[]byte("sealed-cp-package"),
		},
	}

	return arkPSBT, []*psbt.Packet{cpArtifact.PSBT}, recipPolicyBytes,
		routingPkScript, composedPkScript, xfer
}

// TestStartTransferRequestPreparedSubmitTLVRoundTrip verifies that
// StartTransferRequest preserves a PreparedSubmitPackage across durable
// mailbox TLV serialization, including nil float checkpoint asset root slots.
func TestStartTransferRequestPreparedSubmitTLVRoundTrip(t *testing.T) {
	t.Parallel()

	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	senderPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	recipPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	floatPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	const exitDelay = uint32(144)
	assetRoot := chainhash.Hash{0xaa}
	cpRoot := chainhash.Hash{0xbb}
	const assetRef = "asset-usd"

	arkPSBT, cps, recipPolicy, _, composedScript, _ :=
		buildTestIncomingAssetGraph(
			t, recipPriv.PubKey(), operatorPriv, exitDelay,
			assetRoot, assetRef, 500,
		)

	floatPolicy, err := arkscript.EncodeStandardVTXOTemplate(
		floatPriv.PubKey(), operatorPriv.PubKey(), exitDelay,
	)
	require.NoError(t, err)
	floatTemplate, err := arkscript.DecodePolicyTemplate(floatPolicy)
	require.NoError(t, err)
	floatPk, err := floatTemplate.PkScript()
	require.NoError(t, err)

	floatCollabLeaf, err := arkscript.MultiSigCollabTapLeaf(
		floatPriv.PubKey(), operatorPriv.PubKey(),
	)
	require.NoError(t, err)

	floatCP, err := oortx.BuildCheckpointPSBT(
		arkscript.CheckpointPolicy{
			OperatorKey: operatorPriv.PubKey(),
			CSVDelay:    exitDelay,
		},
		oortx.CheckpointInput{
			SpentVTXO: oortx.SpentVTXORef{
				Outpoint: wire.OutPoint{
					Hash:  chainhash.Hash{0x33},
					Index: 1,
				},
				Output: wire.NewTxOut(2_000, floatPk),
			},
			OwnerLeafScript: floatCollabLeaf.Script,
		},
	)
	require.NoError(t, err)

	assetInput := newTestComposedAssetInput(
		t, senderPriv, operatorPriv.PubKey(), wire.OutPoint{
			Hash:  chainhash.Hash{0x11},
			Index: 0,
		},
		1_000,
		chainhash.Hash{0x22},
		assetRef,
		500,
		oortx.CarrierOriginRound,
	)

	orig := &StartTransferRequest{
		Policy: arkscript.CheckpointPolicy{
			OperatorKey: operatorPriv.PubKey(),
			CSVDelay:    exitDelay,
		},
		Inputs: []TransferInput{
			assetInput,
		},
		Recipients: []oortx.RecipientOutput{{
			PkScript:           composedScript,
			Value:              1_000,
			VTXOPolicyTemplate: recipPolicy,
			Asset: &oortx.RecipientAsset{
				Root:   assetRoot,
				Ref:    assetRef,
				Amount: 500,
			},
		}},
		PreparedSubmit: &PreparedSubmitPackage{
			ArkPSBT: arkPSBT,
			CheckpointPSBTs: []*psbt.Packet{
				cps[0],
				floatCP.PSBT,
			},
			CheckpointAssetRoots: []*chainhash.Hash{
				&cpRoot, nil,
			},
			TaprootAssetTransfer: &oortx.TaprootAssetTransfer{
				Version:    oortx.TaprootAssetTransferVersion,
				ArkPackage: []byte("sealed-ark"),
				CheckpointPackages: [][]byte{
					[]byte("sealed-cp-0"), nil,
				},
			},
		},
		IdempotencyKey: "asset-send-key-1",
	}

	var buf bytes.Buffer
	require.NoError(t, orig.Encode(&buf))

	var decoded StartTransferRequest
	require.NoError(t, decoded.Decode(bytes.NewReader(buf.Bytes())))
	require.NotNil(t, decoded.PreparedSubmit)
	require.Equal(
		t, orig.PreparedSubmit.ArkPSBT.UnsignedTx.TxHash(),
		decoded.PreparedSubmit.ArkPSBT.UnsignedTx.TxHash(),
	)
	require.Len(t, decoded.PreparedSubmit.CheckpointPSBTs, 2)
	require.Len(t, decoded.PreparedSubmit.CheckpointAssetRoots, 2)
	require.NotNil(t, decoded.PreparedSubmit.CheckpointAssetRoots[0])
	require.Equal(
		t, cpRoot, *decoded.PreparedSubmit.CheckpointAssetRoots[0],
	)
	require.Nil(t, decoded.PreparedSubmit.CheckpointAssetRoots[1])
	require.Equal(
		t, orig.PreparedSubmit.TaprootAssetTransfer,
		decoded.PreparedSubmit.TaprootAssetTransfer,
	)
}

// TestIncomingTransferEventFromResponseAssetValidation verifies asset metadata
// parsing, composed script binding, fail-closed partial field rejection, and
// TLV round-tripping through both DriveEventRequest and IncomingSnapshot.
func TestIncomingTransferEventFromResponseAssetValidation(t *testing.T) {
	t.Parallel()

	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	recipPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	const exitDelay = uint32(144)
	assetRoot := chainhash.Hash{0xcc}
	const assetRef = "asset-usd"
	const assetAmount = uint64(750)

	arkPSBT, cps, recipPolicy, routingScript, composedScript, xfer :=
		buildTestIncomingAssetGraph(
			t, recipPriv.PubKey(), operatorPriv, exitDelay,
			assetRoot, assetRef, assetAmount,
		)

	sessionID := SessionID(arkPSBT.UnsignedTx.TxHash())
	arkRaw, err := psbtutil.Serialize(arkPSBT)
	require.NoError(t, err)
	cpRaw, err := psbtutil.Serialize(cps[0])
	require.NoError(t, err)
	xferRaw, err := xfer.MarshalBinary()
	require.NoError(t, err)

	validEvent := &arkrpc.OORRecipientEvent{
		EventId:            42,
		SessionId:          sessionID[:],
		OutputIndex:        0,
		Value:              1_000,
		RecipientPkScript:  composedScript,
		VtxoPolicyTemplate: recipPolicy,
		ArkPsbt:            arkRaw,
		CheckpointPsbts: [][]byte{
			cpRaw,
		},
		TaprootAssetRoot:     assetRoot[:],
		TaprootAssetRef:      assetRef,
		TaprootAssetAmount:   assetAmount,
		TaprootAssetTransfer: xferRaw,
	}

	incoming, err := IncomingTransferEventFromResponse(
		sessionID, 42,
		&arkrpc.ListOORRecipientEventsByScriptResponse{
			Events: []*arkrpc.OORRecipientEvent{validEvent},
		},
	)
	require.NoError(t, err)
	require.Equal(t, xfer, incoming.TaprootAssetTransfer)
	require.Len(t, incoming.Recipients, 1)
	require.NotNil(t, incoming.Recipients[0].Asset)
	require.Equal(t, assetRoot, incoming.Recipients[0].Asset.Root)
	require.Equal(t, assetRef, incoming.Recipients[0].Asset.Ref)
	require.Equal(t, assetAmount, incoming.Recipients[0].Asset.Amount)

	gotRouting, err := incoming.Recipients[0].RoutingPkScript()
	require.NoError(t, err)
	require.Equal(t, routingScript, gotRouting)

	// Round-trip IncomingTransferEvent via DriveEventRequest TLV.
	driveReq := &DriveEventRequest{
		SessionID: sessionID,
		Event:     incoming,
	}
	var driveBuf bytes.Buffer
	require.NoError(t, driveReq.Encode(&driveBuf))
	var decodedDrive DriveEventRequest
	require.NoError(
		t,
		decodedDrive.Decode(
			bytes.NewReader(
				driveBuf.Bytes(),
			),
		),
	)
	decodedIncoming, ok := decodedDrive.Event.(*IncomingTransferEvent)
	require.True(t, ok)
	require.Equal(t, xfer, decodedIncoming.TaprootAssetTransfer)
	require.Len(t, decodedIncoming.Recipients, 1)
	require.Equal(
		t, incoming.Recipients[0].Asset,
		decodedIncoming.Recipients[0].Asset,
	)

	// Round-trip ReceiveNotified through IncomingSnapshot TLV.
	notified := &ReceiveNotified{
		SessionID:            sessionID,
		ArkPSBT:              arkPSBT,
		FinalCheckpointPSBTs: cps,
		Recipients:           incoming.Recipients,
		TaprootAssetTransfer: incoming.TaprootAssetTransfer,
	}
	snap, err := NewIncomingSnapshot(sessionID, notified)
	require.NoError(t, err)
	snapRaw, err := encodeIncomingSnapshot(snap)
	require.NoError(t, err)
	decodedSnap, err := decodeIncomingSnapshotWithLimits(
		snapRaw, ReceiveLimits{},
	)
	require.NoError(t, err)
	restoredState, err := IncomingStateFromSnapshot(decodedSnap)
	require.NoError(t, err)
	restoredNotified, ok := restoredState.(*ReceiveNotified)
	require.True(t, ok)
	require.Equal(t, xfer, restoredNotified.TaprootAssetTransfer)
	require.Equal(
		t, incoming.Recipients[0].Asset,
		restoredNotified.Recipients[0].Asset,
	)

	// Partial asset fields (missing TaprootAssetTransfer) must fail closed.
	missingXfer, ok := proto.Clone(validEvent).(*arkrpc.OORRecipientEvent)
	require.True(t, ok)
	missingXfer.TaprootAssetTransfer = nil
	_, err = IncomingTransferEventFromResponse(
		sessionID, 42,
		&arkrpc.ListOORRecipientEventsByScriptResponse{
			Events: []*arkrpc.OORRecipientEvent{missingXfer},
		},
	)
	require.ErrorContains(t, err, "missing taproot asset transfer")

	// Tampered asset root fails composed-script binding check.
	badRoot := chainhash.Hash{0xdd}
	wrongRootEvent, ok := proto.Clone(
		validEvent,
	).(*arkrpc.OORRecipientEvent)
	require.True(t, ok)
	wrongRootEvent.TaprootAssetRoot = badRoot[:]
	_, err = IncomingTransferEventFromResponse(
		sessionID, 42,
		&arkrpc.ListOORRecipientEventsByScriptResponse{
			Events: []*arkrpc.OORRecipientEvent{wrongRootEvent},
		},
	)
	require.ErrorContains(t, err, "does not match pkscript")
}

// TestLocalPersistenceHandlerIncomingAssetMaterialization verifies that
// AuthenticateIncomingMetadataRequest runs VerifyIncomingAssetPackage and that
// MaterializeIncomingVTXOsRequest persists TaprootAssetRoot, Ref, Amount, and
// SealedPackage on the materialized VTXO descriptor.
func TestLocalPersistenceHandlerIncomingAssetMaterialization(t *testing.T) {
	t.Parallel()

	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	recipPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	const exitDelay = uint32(144)
	assetRoot := chainhash.Hash{0xee}
	const assetRef = "asset-usd"
	const assetAmount = uint64(600)

	arkPSBT, cps, recipPolicy, routingScript, composedScript, xfer :=
		buildTestIncomingAssetGraph(
			t, recipPriv.PubKey(), operatorPriv, exitDelay,
			assetRoot, assetRef, assetAmount,
		)
	sessionID := SessionID(arkPSBT.UnsignedTx.TxHash())

	recipients := []ArkRecipientOutput{{
		OutputIndex:        0,
		Value:              1_000,
		PkScript:           composedScript,
		VTXOPolicyTemplate: recipPolicy,
		Asset: &oortx.RecipientAsset{
			Root:   assetRoot,
			Ref:    assetRef,
			Amount: assetAmount,
		},
	}}

	commitmentID := chainhash.Hash{0x55}
	baseMatch := IncomingMetadataMatch{
		OutputIndex: 0,
		Metadata: IncomingVTXOMetadata{
			RoundID:        "round-1",
			CommitmentTxID: commitmentID,
			ChainDepth:     1,
			OperatorKey:    operatorPriv.PubKey(),
			Ancestry:       validTestIncomingAncestry(commitmentID),
		},
	}

	store := newTestVTXOStore()
	pkgStore := &testPackageStore{}
	var verifiedCalls int
	var notified []*vtxo.Descriptor

	handler := &LocalPersistenceOutboxHandler{
		Store:        store,
		PackageStore: pkgStore,
		OperatorKey:  operatorPriv.PubKey(),
		ExitDelay:    exitDelay,
		AuthenticateIncomingExpiry: func(_ context.Context,
			_ []vtxo.Ancestry) (vtxo.AuthenticatedBatchEvidence,
			error) {

			return vtxo.AuthenticatedBatchEvidence{
				BatchExpiry:              10_000,
				EarliestCommitmentHeight: 100,
			}, nil
		},
		VerifyIncomingAssetPackage: func(_ context.Context,
			req IncomingAssetVerification) error {

			verifiedCalls++
			require.Equal(t, sessionID, req.SessionID)
			require.Equal(t, assetRef, req.Recipient.Asset.Ref)
			require.Equal(
				t, xfer.ArkPackage,
				req.TaprootAssetTransfer.ArkPackage,
			)

			return nil
		},
		ResolveIncomingClientKey: func(_ context.Context,
			recipient ArkRecipientOutput) (keychain.KeyDescriptor,
			error) {

			gotRouting, err := recipient.RoutingPkScript()
			require.NoError(t, err)
			require.Equal(t, routingScript, gotRouting)

			return keychain.KeyDescriptor{
				PubKey: recipPriv.PubKey(),
			}, nil
		},
		NotifyIncomingVTXOs: func(_ context.Context,
			vtxos []*vtxo.Descriptor) error {

			notified = append(notified, vtxos...)

			return nil
		},
	}

	authEvents, err := handler.Handle(
		t.Context(), sessionID, &AuthenticateIncomingMetadataRequest{
			SessionID: sessionID,
			Matches: []IncomingMetadataMatch{
				baseMatch,
			},
			ArkPSBT:              arkPSBT,
			FinalCheckpointPSBTs: cps,
			Recipients:           recipients,
			TaprootAssetTransfer: xfer,
		},
	)
	require.NoError(t, err)
	require.Equal(t, 1, verifiedCalls)
	require.Len(t, authEvents, 1)

	resolved, ok := authEvents[0].(*IncomingMetadataResolvedEvent)
	require.True(t, ok)
	require.True(t, resolved.ExpiryAuthenticated)
	require.Len(t, resolved.Matches, 1)
	require.True(t, resolved.Matches[0].Metadata.AssetVerified)

	matEvents, err := handler.Handle(
		t.Context(), sessionID, &MaterializeIncomingVTXOsRequest{
			SessionID:            sessionID,
			ArkPSBT:              arkPSBT,
			FinalCheckpointPSBTs: cps,
			Recipients:           recipients,
			MetadataMatches:      resolved.Matches,
			TaprootAssetTransfer: xfer,
		},
	)
	require.NoError(t, err)
	require.Len(t, matEvents, 1)
	require.Len(t, notified, 1)

	op := wire.OutPoint{Hash: chainhash.Hash(sessionID), Index: 0}
	saved, err := store.GetVTXO(t.Context(), op)
	require.NoError(t, err)
	require.Nil(t, saved.TapScript)
	require.NotNil(t, saved.TaprootAssetRoot)
	require.Equal(t, assetRoot, *saved.TaprootAssetRoot)
	require.Equal(t, assetRef, saved.TaprootAssetRef)
	require.Equal(t, assetAmount, saved.TaprootAssetAmount)
	require.Equal(t, xfer.ArkPackage, saved.TaprootAssetSealedPackage)

	// An asset TransferInput loaded from VTXOStore carries TapScript == nil
	// and must still validate, derive its composed spend path, and preserve
	// TapScript == nil across snapshot round-trip.
	assetInput := TransferInput{
		VTXO:                      saved,
		VTXOPolicyTemplate:        recipPolicy,
		TaprootAssetCarrierOrigin: oortx.CarrierOriginOOR,
	}
	require.NoError(t, assetInput.Validate())
	spendPath, err := assetInput.EffectiveSpendPath()
	require.NoError(t, err)
	require.NoError(t, spendPath.VerifyBindsToPkScript(composedScript))

	inputSnap, err := assetInput.ToSnapshot()
	require.NoError(t, err)
	rebuilt, err := TransferInputFromSnapshot(inputSnap)
	require.NoError(t, err)
	require.Nil(t, rebuilt.VTXO.TapScript)
	require.Equal(t, composedScript, rebuilt.VTXO.PkScript)
	require.NoError(t, rebuilt.Validate())

	// A verifier rejection marked with ErrInvalidIncomingAssetPackage fails
	// terminally so the receive FSM does not retry forever.
	handler.VerifyIncomingAssetPackage = func(_ context.Context,
		_ IncomingAssetVerification) error {

		return fmt.Errorf("%w: bad proof",
			ErrInvalidIncomingAssetPackage)
	}
	_, err = handler.Handle(
		t.Context(), sessionID, &AuthenticateIncomingMetadataRequest{
			SessionID: sessionID,
			Matches: []IncomingMetadataMatch{
				baseMatch,
			},
			ArkPSBT:              arkPSBT,
			FinalCheckpointPSBTs: cps,
			Recipients:           recipients,
			TaprootAssetTransfer: xfer,
		},
	)
	require.Error(t, err)
	var termErr *terminalOutboxError
	require.True(t, errors.As(err, &termErr))
}

// TestMultiEventIncomingReceivePass verifies that an incoming OOR session
// records completed recipient event IDs across snapshots, allows a distinct
// recipient event for the same session (e.g. asset change plus Bitcoin carrier
// return) to run a fresh receive pass after ReceiveCompleted, postpones a
// distinct event arriving while another pass is in flight, absorbs duplicates,
// and preserves multiple parked self-hints per session in FIFO order.
func TestMultiEventIncomingReceivePass(t *testing.T) {
	t.Parallel()

	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	recipPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	arkPSBT, cps, _, routingScript, _, xfer := buildTestIncomingAssetGraph(
		t, recipPriv.PubKey(), operatorPriv, 144, chainhash.Hash{0xaa},
		"asset-usd", 150,
	)
	sessionID := SessionID(arkPSBT.UnsignedTx.TxHash())

	// Walk ReceiveResolving(event 3) -> ReceiveNotified ->
	// ReceiveAwaitingAck -> ReceiveCompleted{CompletedEventIDs: [3]}.
	sess, err := newReceiveSessionWithState(
		t.Context(), sessionID, &ReceiveResolving{
			SessionID:         sessionID,
			RecipientPkScript: routingScript,
			RecipientEventID:  3,
		}, EnvConfig{},
	)
	require.NoError(t, err)

	res := sess.FSM.AskEvent(t.Context(), &IncomingTransferEvent{
		SessionID:            sessionID,
		ArkPSBT:              arkPSBT,
		FinalCheckpointPSBTs: cps,
		TaprootAssetTransfer: xfer,
	}).Await(t.Context())
	require.NoError(t, res.Err())

	st, err := sess.FSM.CurrentState()
	require.NoError(t, err)
	notified, ok := st.(*ReceiveNotified)
	require.True(t, ok)
	require.EqualValues(t, 3, notified.RecipientEventID)

	res = sess.FSM.AskEvent(t.Context(), &IncomingHandledEvent{}).
		Await(t.Context())
	require.NoError(t, res.Err())

	res = sess.FSM.AskEvent(t.Context(), &IncomingAckSentEvent{}).
		Await(t.Context())
	require.NoError(t, res.Err())

	st, err = sess.FSM.CurrentState()
	require.NoError(t, err)
	completed, ok := st.(*ReceiveCompleted)
	require.True(t, ok)
	require.Equal(t, []uint64{3}, completed.CompletedEventIDs)
	require.True(t, completed.HasEventID(3))
	require.False(t, completed.HasEventID(2))

	// Snapshot round-trip preserves CompletedEventIDs on ReceiveCompleted.
	snap, err := NewIncomingSnapshot(sessionID, completed)
	require.NoError(t, err)
	raw, err := encodeIncomingSnapshot(snap)
	require.NoError(t, err)
	decodedSnap, err := decodeIncomingSnapshotWithLimits(
		raw, ReceiveLimits{},
	)
	require.NoError(t, err)
	restoredState, err := IncomingStateFromSnapshot(decodedSnap)
	require.NoError(t, err)
	restoredCompleted, ok := restoredState.(*ReceiveCompleted)
	require.True(t, ok)
	require.Equal(t, []uint64{3}, restoredCompleted.CompletedEventIDs)

	// On the session behavior: repeating event 3 is a no-op; a distinct
	// event 2 starts a fresh ReceiveResolving pass carrying [3]; and while
	// event 2 is active, a distinct event 4 postpones instead of dropping.
	b := &sessionBehavior{
		sessionID: sessionID,
		log:       btclog.Disabled,
		loaded:    true,
	}
	b.setFSM(sess.FSM)

	dupRes := b.handleResolveIncomingTransfer(
		t.Context(), &ResolveIncomingTransferRequest{
			SessionID:         sessionID,
			RecipientPkScript: routingScript,
			RecipientEventID:  3,
		},
	)
	require.NoError(t, dupRes.Err())
	st, err = b.fsm.CurrentState()
	require.NoError(t, err)
	_, stillCompleted := st.(*ReceiveCompleted)
	require.True(t, stillCompleted)

	nextRes := b.handleResolveIncomingTransfer(
		t.Context(), &ResolveIncomingTransferRequest{
			SessionID:         sessionID,
			RecipientPkScript: routingScript,
			RecipientEventID:  2,
		},
	)
	require.NoError(t, nextRes.Err())
	st, err = b.fsm.CurrentState()
	require.NoError(t, err)
	resolving, ok := st.(*ReceiveResolving)
	require.True(t, ok)
	require.EqualValues(t, 2, resolving.RecipientEventID)
	require.Equal(t, []uint64{3}, resolving.CompletedEventIDs)

	busyRes := b.handleResolveIncomingTransfer(
		t.Context(), &ResolveIncomingTransferRequest{
			SessionID:         sessionID,
			RecipientPkScript: routingScript,
			RecipientEventID:  4,
		},
	)
	require.ErrorIs(t, busyRes.Err(), actor.ErrPostponed)

	// Registry parkedSelfHints preserves multiple distinct recipient
	// events for the same session in FIFO order and deduplicates repeats.
	reg := &oorRegistryBehavior{}
	reg.parkSelfHint(&ResolveIncomingTransferRequest{
		SessionID:         sessionID,
		RecipientPkScript: routingScript,
		RecipientEventID:  2,
	})
	reg.parkSelfHint(&ResolveIncomingTransferRequest{
		SessionID:         sessionID,
		RecipientPkScript: routingScript,
		RecipientEventID:  3,
	})
	reg.parkSelfHint(&ResolveIncomingTransferRequest{
		SessionID:         sessionID,
		RecipientPkScript: routingScript,
		RecipientEventID:  2,
	})
	require.Len(t, reg.parkedSelfHints[sessionID], 2)
	require.EqualValues(
		t, 2, reg.parkedSelfHints[sessionID][0].
			RecipientEventID,
	)
	require.EqualValues(
		t, 3, reg.parkedSelfHints[sessionID][1].
			RecipientEventID,
	)

	reg.unparkSelfHint(sessionID, 2)
	require.Len(t, reg.parkedSelfHints[sessionID], 1)
	require.EqualValues(
		t, 3, reg.parkedSelfHints[sessionID][0].
			RecipientEventID,
	)
}
