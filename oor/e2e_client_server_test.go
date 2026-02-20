package oor_test

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/lib/scripts"
	oortx "github.com/lightninglabs/darepo-client/lib/tx/oor"
	"github.com/lightninglabs/darepo-client/lib/tx/psbtutil"
	clientoor "github.com/lightninglabs/darepo-client/oor"
	clientvtxo "github.com/lightninglabs/darepo-client/vtxo"
	"github.com/lightninglabs/darepo/db"
	serveroor "github.com/lightninglabs/darepo/oor"
	"github.com/lightninglabs/darepo/vtxo"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// newClientTransferInput constructs a minimally valid transfer input for
// in-process E2E tests.
func newClientTransferInput(t *testing.T, clientKey *btcec.PrivateKey,
	operatorKey *btcec.PublicKey, exitDelay uint32,
	outpoint wire.OutPoint,
	amount btcutil.Amount) clientoor.TransferInput {

	t.Helper()

	tapKey, err := scripts.VTXOTapKey(
		clientKey.PubKey(), operatorKey, exitDelay,
	)
	require.NoError(t, err)

	pkScript, err := txscript.PayToTaprootScript(tapKey)
	require.NoError(t, err)

	tapscript, err := scripts.VTXOTapScript(
		clientKey.PubKey(), operatorKey, exitDelay,
	)
	require.NoError(t, err)

	return clientoor.TransferInput{
		VTXO: &clientvtxo.Descriptor{
			Outpoint: outpoint,
			Amount:   amount,
			PkScript: pkScript,
			ClientKey: keychain.KeyDescriptor{
				PubKey: clientKey.PubKey(),
			},
			OperatorKey:    operatorKey,
			TapScript:      tapscript,
			RelativeExpiry: exitDelay,
			Status:         clientvtxo.VTXOStatusLive,
		},
		OwnerLeafScript: []byte{0x51},
	}
}

// finalizeCheckpointPSBTsForTest clones co-signed checkpoints and
// attaches client signatures so the finalize package is spendable and
// deterministic.
func finalizeCheckpointPSBTsForTest(t *testing.T,
	clientSigner input.Signer, inputs []clientoor.TransferInput,
	coSigned []*psbt.Packet) []*psbt.Packet {

	t.Helper()

	finalized := make([]*psbt.Packet, 0, len(coSigned))
	for _, pkt := range coSigned {
		require.NotNil(t, pkt)
		require.NotNil(t, pkt.UnsignedTx)

		raw, err := psbtutil.Serialize(pkt)
		require.NoError(t, err)

		finalPkt, err := psbtutil.Parse(raw)
		require.NoError(t, err)
		require.NotEmpty(t, finalPkt.Inputs)
		finalized = append(finalized, finalPkt)
	}

	err := clientoor.SignCheckpointPSBTs(clientSigner, inputs, finalized)
	require.NoError(t, err)

	return finalized
}

// startE2EServerActor creates and starts a server actor for E2E tests,
// registering cleanup to stop it when the test finishes.
func startE2EServerActor(t *testing.T,
	cfg serveroor.ActorCfg) *serveroor.Actor {

	t.Helper()

	server := serveroor.NewActor(cfg)

	err := server.Start(t.Context())
	require.NoError(t, err)

	t.Cleanup(server.Stop)

	return server
}

// attachArkLeafScriptsForTest attaches the owner leaf spend path to
// each Ark input so submit package validation can reconstruct a
// spendable witness.
//
// The Ark inputs are keyed by checkpoint txid while transfer inputs are
// keyed by spent VTXO outpoint. This helper bridges them through the
// checkpoint mapping and uses the embedded tap tree metadata on the Ark
// PSBT inputs to derive the concrete tapleaf path.
func attachArkLeafScriptsForTest(t *testing.T, ark *psbt.Packet,
	transferInputs []clientoor.TransferInput,
	checkpoints []*psbt.Packet) {

	t.Helper()

	require.NotNil(t, ark)
	require.NotNil(t, ark.UnsignedTx)
	require.NotEmpty(t, transferInputs)
	require.NotEmpty(t, checkpoints)
	require.Len(t, ark.Inputs, len(ark.UnsignedTx.TxIn))

	inputBySpentOutpoint := make(
		map[wire.OutPoint]clientoor.TransferInput,
		len(transferInputs),
	)
	for i := range transferInputs {
		in := transferInputs[i]
		require.NoError(t, in.Validate())
		inputBySpentOutpoint[in.VTXO.Outpoint] = in
	}

	leafByCheckpointTxid := make(
		map[chainhash.Hash][]byte, len(checkpoints),
	)
	for i := range checkpoints {
		checkpoint := checkpoints[i]
		require.NotNil(t, checkpoint)
		require.NotNil(t, checkpoint.UnsignedTx)
		require.Len(t, checkpoint.UnsignedTx.TxIn, 1)

		spentOutpoint := checkpoint.UnsignedTx.
			TxIn[0].PreviousOutPoint

		transferInput, ok := inputBySpentOutpoint[spentOutpoint]
		require.True(
			t, ok,
			"missing transfer input for spent outpoint",
		)

		checkpointTxid := checkpoint.UnsignedTx.TxHash()
		leafByCheckpointTxid[checkpointTxid] = append(
			[]byte(nil),
			transferInput.OwnerLeafScript...,
		)
	}

	for i := range ark.Inputs {
		prevOut := ark.UnsignedTx.TxIn[i].PreviousOutPoint
		ownerLeaf, ok := leafByCheckpointTxid[prevOut.Hash]
		require.True(
			t, ok, "missing owner leaf for ark input",
		)

		tapTreeEncoded, err := oortx.GetTapTreePSBTInput(
			ark.Inputs[i],
		)
		require.NoError(t, err)

		leaf, err := oortx.BuildTaprootTapLeafScript(
			tapTreeEncoded, ownerLeaf,
		)
		require.NoError(t, err)

		ark.Inputs[i].TaprootLeafScript =
			[]*psbt.TaprootTapLeafScript{leaf}
	}
}

// clonePSBTSliceForTest deep-copies a PSBT slice via serialize/parse
// so tests do not share mutable packet pointers across actor
// boundaries.
func clonePSBTSliceForTest(t *testing.T,
	packets []*psbt.Packet) []*psbt.Packet {

	t.Helper()

	clones := make([]*psbt.Packet, 0, len(packets))
	for i := range packets {
		raw, err := psbtutil.Serialize(packets[i])
		require.NoError(t, err)

		clone, err := psbtutil.Parse(raw)
		require.NoError(t, err)

		clones = append(clones, clone)
	}

	return clones
}

// inProcessClientToServerOutbox is a test-only adaptor that connects
// the client OOR FSM outbox messages to the server OOR coordinator
// actor, without RPC.
type inProcessClientToServerOutbox struct {
	t *testing.T

	server    *serveroor.Actor
	sessionID serveroor.SessionID

	lastFinalizeArkPSBT *psbt.Packet

	signDescs    []serveroor.VTXOSigningDescriptor
	clientSigner input.Signer
}

// Handle processes a client outbox request and returns follow-up
// events.
func (h *inProcessClientToServerOutbox) Handle(ctx context.Context,
	sessionID clientoor.SessionID,
	outbox clientoor.OutboxEvent) ([]clientoor.Event, error) {

	h.t.Helper()

	switch msg := outbox.(type) {
	case *clientoor.SendSubmitPackageRequest:
		attachArkLeafScriptsForTest(
			h.t, msg.ArkPSBT, msg.TransferInputs,
			msg.CheckpointPSBTs,
		)

		resp := h.server.Receive(
			ctx, &serveroor.SubmitOORRequest{
				ArkPSBT:                msg.ArkPSBT,
				CheckpointPSBTs:        msg.CheckpointPSBTs,
				VTXOSigningDescriptors: h.signDescs,
			},
		)
		require.True(h.t, resp.IsOk(), resp.Err())

		unwrapped := resp.UnwrapOr(nil)
		serverMsg, ok := unwrapped.(*serveroor.SubmitOORResponse)
		require.True(h.t, ok)
		h.sessionID = serverMsg.SessionID

		coSigned := clonePSBTSliceForTest(
			h.t, serverMsg.CoSignedCheckpointPSBTs,
		)

		return []clientoor.Event{
			&clientoor.SubmitAcceptedEvent{
				SessionID: clientoor.SessionID(
					serverMsg.SessionID,
				),
				ArkPSBT:                 msg.ArkPSBT,
				CoSignedCheckpointPSBTs: coSigned,
			},
		}, nil

	case *clientoor.RequestCheckpointSignatures:
		finalCheckpointPSBTs := finalizeCheckpointPSBTsForTest(
			h.t, h.clientSigner, msg.TransferInputs,
			msg.CoSignedCheckpointPSBTs,
		)

		return []clientoor.Event{
			&clientoor.CheckpointsSignedEvent{
				FinalCheckpointPSBTs: finalCheckpointPSBTs,
			},
		}, nil

	case *clientoor.SendFinalizePackageRequest:
		if msg.ArkPSBT == nil || msg.ArkPSBT.UnsignedTx == nil {
			return nil, fmt.Errorf(
				"ark psbt must be provided",
			)
		}

		h.lastFinalizeArkPSBT = msg.ArkPSBT

		resp := h.server.Receive(
			ctx, &serveroor.FinalizeOORRequest{
				SessionID: serveroor.SessionID(
					sessionID,
				),
				FinalCheckpointPSBTs: msg.
					FinalCheckpointPSBTs,
			},
		)
		require.True(h.t, resp.IsOk())

		_, ok := resp.UnwrapOr(nil).(*serveroor.FinalizeOORResponse)
		require.True(h.t, ok)

		return []clientoor.Event{
			&clientoor.FinalizeAcceptedEvent{},
		}, nil

	case *clientoor.MarkInputsSpentRequest:
		// Outgoing OOR transfers are off-chain. After finalize
		// is accepted, the client updates its local VTXO
		// persistence state and then completes the session.
		_ = msg
		return []clientoor.Event{
			&clientoor.InputsMarkedSpentEvent{},
		}, nil

	default:
		return nil, nil
	}
}

var _ clientoor.OutboxHandler = (*inProcessClientToServerOutbox)(nil)

// inProcessReceiveOutbox is a test-only outbox handler for incoming
// transfer sessions.
type inProcessReceiveOutbox struct {
	t *testing.T

	recipientKey keychain.KeyDescriptor

	operatorKey *btcec.PublicKey

	exitDelay uint32

	materialized []*clientvtxo.Descriptor
}

// Handle processes incoming-transfer outbox messages and returns
// follow-ups.
func (h *inProcessReceiveOutbox) Handle(_ context.Context,
	_ clientoor.SessionID,
	outbox clientoor.OutboxEvent) ([]clientoor.Event, error) {

	h.t.Helper()

	switch msg := outbox.(type) {
	case *clientoor.IncomingTransferNotification:
		return nil, nil

	case *clientoor.MaterializeIncomingVTXOsRequest:
		arkTxid := msg.ArkPSBT.UnsignedTx.TxHash()

		for i := range msg.Recipients {
			recipient := msg.Recipients[i]
			desc := &clientvtxo.Descriptor{
				Outpoint: wire.OutPoint{
					Hash:  arkTxid,
					Index: recipient.OutputIndex,
				},
				Amount:         recipient.Value,
				PkScript:       recipient.PkScript,
				ClientKey:      h.recipientKey,
				OperatorKey:    h.operatorKey,
				RelativeExpiry: h.exitDelay,
				RoundID:        "oor-e2e",
				CommitmentTxID: arkTxid,
				Status:         clientvtxo.VTXOStatusLive,
			}
			h.materialized = append(
				h.materialized, desc,
			)
		}

		return []clientoor.Event{
			&clientoor.IncomingHandledEvent{},
		}, nil

	case *clientoor.SendIncomingAckRequest:
		return nil, nil

	default:
		return nil, nil
	}
}

var _ clientoor.OutboxHandler = (*inProcessReceiveOutbox)(nil)

// driveOutboxToFSM drives an outbox list by invoking the handler and
// feeding any follow-up events back into the FSM until no more outbox
// events are emitted.
func driveOutboxToFSM(ctx context.Context, t *testing.T,
	sessionID clientoor.SessionID, fsm *clientoor.StateMachine,
	handler clientoor.OutboxHandler,
	outbox []clientoor.OutboxEvent) error {

	t.Helper()

	for _, msg := range outbox {
		followUps, err := handler.Handle(ctx, sessionID, msg)
		if err != nil {
			return err
		}

		for _, evt := range followUps {
			fut := fsm.AskEvent(ctx, evt)
			result := fut.Await(ctx)
			if result.IsErr() {
				return result.Err()
			}

			next := result.UnwrapOr(nil)
			err = driveOutboxToFSM(
				ctx, t, sessionID, fsm,
				handler, next,
			)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

// pausedFinalizeAdaptor is an outbox handler that drives submit and
// checkpoint signing, but records finalize requests so the test can
// simulate a server restart before finalization.
type pausedFinalizeAdaptor struct {
	t *testing.T

	server *serveroor.Actor

	pauseFinalize bool

	sessionID clientoor.SessionID

	arkPSBT *psbt.Packet

	finalCheckpointPSBTs []*psbt.Packet

	signDescs    []serveroor.VTXOSigningDescriptor
	clientSigner input.Signer
}

// Handle processes the outbox request and returns follow-up events.
func (h *pausedFinalizeAdaptor) Handle(ctx context.Context,
	sessionID clientoor.SessionID,
	outbox clientoor.OutboxEvent) ([]clientoor.Event, error) {

	h.t.Helper()

	switch msg := outbox.(type) {
	case *clientoor.SendSubmitPackageRequest:
		attachArkLeafScriptsForTest(
			h.t, msg.ArkPSBT, msg.TransferInputs,
			msg.CheckpointPSBTs,
		)

		resp := h.server.Receive(
			ctx, &serveroor.SubmitOORRequest{
				ArkPSBT:                msg.ArkPSBT,
				CheckpointPSBTs:        msg.CheckpointPSBTs,
				VTXOSigningDescriptors: h.signDescs,
			},
		)
		require.True(h.t, resp.IsOk())

		unwrapped := resp.UnwrapOr(nil)
		serverMsg, ok := unwrapped.(*serveroor.SubmitOORResponse)
		require.True(h.t, ok)

		h.sessionID = clientoor.SessionID(
			serverMsg.SessionID,
		)

		coSigned := clonePSBTSliceForTest(
			h.t, serverMsg.CoSignedCheckpointPSBTs,
		)

		return []clientoor.Event{
			&clientoor.SubmitAcceptedEvent{
				SessionID:               h.sessionID,
				ArkPSBT:                 msg.ArkPSBT,
				CoSignedCheckpointPSBTs: coSigned,
			},
		}, nil

	case *clientoor.RequestCheckpointSignatures:
		finalCheckpointPSBTs := finalizeCheckpointPSBTsForTest(
			h.t, h.clientSigner, msg.TransferInputs,
			msg.CoSignedCheckpointPSBTs,
		)

		return []clientoor.Event{
			&clientoor.CheckpointsSignedEvent{
				FinalCheckpointPSBTs: finalCheckpointPSBTs,
			},
		}, nil

	case *clientoor.SendFinalizePackageRequest:
		if msg.ArkPSBT == nil || msg.ArkPSBT.UnsignedTx == nil {
			return nil, fmt.Errorf(
				"ark psbt must be provided",
			)
		}

		h.arkPSBT = msg.ArkPSBT
		h.finalCheckpointPSBTs = msg.FinalCheckpointPSBTs

		if h.pauseFinalize {
			// Do not acknowledge finalize yet. This simulates
			// an async RPC send where the response arrives
			// later (after restart).
			return nil, nil
		}

		resp := h.server.Receive(
			ctx, &serveroor.FinalizeOORRequest{
				SessionID: serveroor.SessionID(
					sessionID,
				),
				FinalCheckpointPSBTs: msg.
					FinalCheckpointPSBTs,
			},
		)
		require.True(h.t, resp.IsOk(), resp.Err())

		_, ok := resp.UnwrapOr(nil).(*serveroor.FinalizeOORResponse)
		require.True(h.t, ok)

		return []clientoor.Event{
			&clientoor.FinalizeAcceptedEvent{},
		}, nil

	case *clientoor.MarkInputsSpentRequest:
		_ = msg
		return []clientoor.Event{
			&clientoor.InputsMarkedSpentEvent{},
		}, nil

	default:
		return nil, nil
	}
}

var _ clientoor.OutboxHandler = (*pausedFinalizeAdaptor)(nil)

// TestOORClientServerE2E asserts the client outgoing transfer FSM can
// drive the server coordinator FSM to finalized, using only in-process
// test adaptors.
func TestOORClientServerE2E(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	sqlStore := db.NewTestDB(t)
	dbStore := db.NewStore(
		sqlStore.DB, sqlStore.Queries, sqlStore.Backend(),
		btclog.Disabled, clock.NewDefaultClock(),
	)
	locker := db.NewVTXOLockerDB(dbStore, btclog.Disabled)
	store := dbStore.NewVTXORecordStore()

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	operatorSigner := input.NewMockSigner(
		[]*btcec.PrivateKey{operatorKey}, nil,
	)

	policy := scripts.CheckpointPolicy{
		OperatorKey: operatorKey.PubKey(),
		CSVDelay:    10,
	}

	driver := serveroor.NewDriver(serveroor.DriverCfg{
		Locker:         locker,
		Store:          store,
		OperatorSigner: operatorSigner,
		OperatorKey: keychain.KeyDescriptor{
			PubKey: operatorKey.PubKey(),
		},
	})

	deliveryStore, err := db.NewActorDeliveryStoreFromDB(
		sqlStore, clock.NewDefaultClock(), btclog.Disabled,
	)
	require.NoError(t, err)

	server := startE2EServerActor(t, serveroor.ActorCfg{
		OutboxHandler:    driver,
		CheckpointPolicy: policy,
		DeliveryStore:    deliveryStore,
	})

	inputValue := btcutil.Amount(10000)

	exitDelay := uint32(10)

	senderKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	clientSigner := input.NewMockSigner(
		[]*btcec.PrivateKey{senderKey}, nil,
	)

	senderInputOutpoint := wire.OutPoint{
		Hash:  [32]byte{0x01},
		Index: 0,
	}

	inputs := []clientoor.TransferInput{
		newClientTransferInput(
			t, senderKey, policy.OperatorKey, exitDelay,
			senderInputOutpoint, inputValue,
		),
	}

	recipientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	recipientTapKey, err := scripts.VTXOTapKey(
		recipientKey.PubKey(), policy.OperatorKey, exitDelay,
	)
	require.NoError(t, err)

	recipientPkScript, err := txscript.PayToTaprootScript(
		recipientTapKey,
	)
	require.NoError(t, err)

	recipients := []oortx.RecipientOutput{
		{
			PkScript: recipientPkScript,
			Value:    inputValue,
		},
	}

	err = store.Create(ctx, &vtxo.Record{
		Outpoint: senderInputOutpoint,
		Value:    int64(inputValue),
		PkScript: inputs[0].VTXO.PkScript,
		Status:   vtxo.StatusLive,
	})
	require.NoError(t, err)

	adaptor := &inProcessClientToServerOutbox{
		t:            t,
		server:       server,
		clientSigner: clientSigner,
		signDescs: []serveroor.VTXOSigningDescriptor{
			{
				Outpoint:  senderInputOutpoint,
				OwnerKey:  senderKey.PubKey(),
				ExitDelay: exitDelay,
			},
		},
	}
	clientDeliveryStore, err := db.NewActorDeliveryStoreFromDB(
		sqlStore, clock.NewDefaultClock(), btclog.Disabled,
	)
	require.NoError(t, err)

	client := clientoor.NewOORClientActor(clientoor.ClientActorCfg{
		OutboxHandler: adaptor,
		DeliveryStore: clientDeliveryStore,
	})
	defer client.Stop()

	startResp := client.Receive(ctx, &clientoor.StartTransferRequest{
		Policy:     policy,
		Inputs:     inputs,
		Recipients: recipients,
	})
	require.True(t, startResp.IsOk(), startResp.Err())

	startUnwrapped := startResp.UnwrapOr(nil)
	startMsg, ok := startUnwrapped.(*clientoor.StartTransferResponse)
	require.True(t, ok)

	// Now simulate the recipient client processing an incoming
	// transfer.
	//
	// In production, this would arrive via RPC push or polling. For
	// now, we drive the incoming-transfer FSM directly.
	require.NotNil(t, adaptor.lastFinalizeArkPSBT)

	receiveSess, receiveOutbox, err := clientoor.DriveIncomingTransfer(
		ctx, startMsg.SessionID, adaptor.lastFinalizeArkPSBT,
	)
	require.NoError(t, err)

	receiveHandler := &inProcessReceiveOutbox{
		t: t,
		recipientKey: keychain.KeyDescriptor{
			PubKey: recipientKey.PubKey(),
		},
		operatorKey: policy.OperatorKey,
		exitDelay:   exitDelay,
	}
	err = driveOutboxToFSM(
		ctx, t, startMsg.SessionID,
		receiveSess.FSM, receiveHandler, receiveOutbox,
	)
	require.NoError(t, err)

	recvState, err := receiveSess.FSM.CurrentState()
	require.NoError(t, err)
	require.IsType(t, &clientoor.ReceiveCompleted{}, recvState)

	require.Len(t, receiveHandler.materialized, 1)
	materialized := receiveHandler.materialized[0]
	require.Equal(t, recipientPkScript, materialized.PkScript)

	// Assert VTXO lifecycle transitions are applied by the server
	// outbox driver:
	//   - input is spent
	//   - recipient output is materialized as live
	inRec, err := store.Get(ctx, senderInputOutpoint)
	require.NoError(t, err)
	require.NotNil(t, inRec)
	require.Equal(t, vtxo.StatusSpent, inRec.Status)

	require.NotNil(t, adaptor.lastFinalizeArkPSBT)
	arkTxid := adaptor.lastFinalizeArkPSBT.UnsignedTx.TxHash()
	outRec, err := store.Get(ctx, wire.OutPoint{
		Hash:  arkTxid,
		Index: 0,
	})
	require.NoError(t, err)
	require.NotNil(t, outRec)
	require.Equal(t, vtxo.StatusLive, outRec.Status)
}

// TestOORClientServerRestartBeforeFinalize asserts a server can restart
// after reaching point-of-no-return and still accept finalize once the
// client resumes.
func TestOORClientServerRestartBeforeFinalize(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	db1 := db.NewTestDB(t)
	sessionStore1 := serveroor.NewDBSessionStore(
		db1, clock.NewDefaultClock(), btclog.Disabled,
	)
	deliveryStore1, err := db.NewActorDeliveryStoreFromDB(
		db1, clock.NewDefaultClock(), btclog.Disabled,
	)
	require.NoError(t, err)

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	operatorSigner := input.NewMockSigner(
		[]*btcec.PrivateKey{operatorKey}, nil,
	)

	policy := scripts.CheckpointPolicy{
		OperatorKey: operatorKey.PubKey(),
		CSVDelay:    10,
	}

	driver1 := serveroor.NewDriver(serveroor.DriverCfg{
		SessionStore:   sessionStore1,
		OperatorSigner: operatorSigner,
		OperatorKey: keychain.KeyDescriptor{
			PubKey: operatorKey.PubKey(),
		},
	})
	server1 := serveroor.NewActor(serveroor.ActorCfg{
		OutboxHandler:    driver1,
		CheckpointPolicy: policy,
		DeliveryStore:    deliveryStore1,
		SessionStore:     sessionStore1,
	})

	err = server1.Start(ctx)
	require.NoError(t, err)

	inputValue := btcutil.Amount(10000)
	exitDelay := uint32(10)

	senderKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	clientSigner := input.NewMockSigner(
		[]*btcec.PrivateKey{senderKey}, nil,
	)

	senderInputOutpoint := wire.OutPoint{
		Hash:  [32]byte{0x02},
		Index: 0,
	}

	inputs := []clientoor.TransferInput{
		newClientTransferInput(
			t, senderKey, policy.OperatorKey, exitDelay,
			senderInputOutpoint, inputValue,
		),
	}

	recipientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	recipientTapKey, err := scripts.VTXOTapKey(
		recipientKey.PubKey(), policy.OperatorKey, exitDelay,
	)
	require.NoError(t, err)

	recipientPkScript, err := txscript.PayToTaprootScript(
		recipientTapKey,
	)
	require.NoError(t, err)

	recipients := []oortx.RecipientOutput{
		{
			PkScript: recipientPkScript,
			Value:    inputValue,
		},
	}

	adaptor := &pausedFinalizeAdaptor{
		t:             t,
		server:        server1,
		pauseFinalize: true,
		clientSigner:  clientSigner,
		signDescs: []serveroor.VTXOSigningDescriptor{
			{
				Outpoint:  senderInputOutpoint,
				OwnerKey:  senderKey.PubKey(),
				ExitDelay: exitDelay,
			},
		},
	}
	clientDeliveryStore, err := db.NewActorDeliveryStoreFromDB(
		db1, clock.NewDefaultClock(), btclog.Disabled,
	)
	require.NoError(t, err)

	client := clientoor.NewOORClientActor(clientoor.ClientActorCfg{
		OutboxHandler: adaptor,
		DeliveryStore: clientDeliveryStore,
	})
	defer client.Stop()

	startResp := client.Receive(
		ctx, &clientoor.StartTransferRequest{
			Policy:     policy,
			Inputs:     inputs,
			Recipients: recipients,
		},
	)
	require.True(t, startResp.IsOk(), startResp.Err())

	startUnwrapped := startResp.UnwrapOr(nil)
	startMsg, ok := startUnwrapped.(*clientoor.StartTransferResponse)
	require.True(t, ok)
	require.Equal(t, adaptor.sessionID, startMsg.SessionID)

	// At this point the server should be at point-of-no-return and
	// the session snapshot should be durable.
	serverState, err := server1.CurrentState(
		ctx, serveroor.SessionID(startMsg.SessionID),
	)
	require.NoError(t, err)
	require.IsType(t, &serveroor.CoSignedState{}, serverState)

	// Simulate server restart by creating a new actor instance that
	// rehydrates state from the database, rather than in-memory
	// session state.
	server1.Stop()

	sessionStore2 := serveroor.NewDBSessionStore(
		db1, clock.NewDefaultClock(), btclog.Disabled,
	)
	deliveryStore2, err := db.NewActorDeliveryStoreFromDB(
		db1, clock.NewDefaultClock(), btclog.Disabled,
	)
	require.NoError(t, err)

	driver2 := serveroor.NewDriver(serveroor.DriverCfg{
		SessionStore:   sessionStore2,
		OperatorSigner: operatorSigner,
		OperatorKey: keychain.KeyDescriptor{
			PubKey: operatorKey.PubKey(),
		},
	})
	server2 := serveroor.NewActor(serveroor.ActorCfg{
		OutboxHandler:    driver2,
		CheckpointPolicy: policy,
		DeliveryStore:    deliveryStore2,
		SessionStore:     sessionStore2,
	})

	err = server2.Start(ctx)
	require.NoError(t, err)
	defer server2.Stop()

	// The paused adaptor captured the finalize package that must
	// survive restart and be replayable.
	require.NotNil(t, adaptor.arkPSBT)
	require.NotEmpty(t, adaptor.finalCheckpointPSBTs)

	// Resume finalize against the restarted server.
	adaptor.server = server2
	adaptor.pauseFinalize = false

	finalizeResp := server2.Receive(
		ctx, &serveroor.FinalizeOORRequest{
			SessionID: serveroor.SessionID(
				startMsg.SessionID,
			),
			FinalCheckpointPSBTs: adaptor.
				finalCheckpointPSBTs,
		},
	)
	require.True(t, finalizeResp.IsOk(), finalizeResp.Err())

	// Drive the local client completion path after finalize
	// acceptance.
	driveResp := client.Receive(ctx, &clientoor.DriveEventRequest{
		SessionID: startMsg.SessionID,
		Event:     &clientoor.FinalizeAcceptedEvent{},
	})
	require.True(t, driveResp.IsOk(), driveResp.Err())

	stateResp := client.Receive(ctx, &clientoor.GetStateRequest{
		SessionID: startMsg.SessionID,
	})
	require.True(t, stateResp.IsOk())

	stateMsg, ok := stateResp.UnwrapOr(
		nil,
	).(*clientoor.GetStateResponse)
	require.True(t, ok)
	require.IsType(t, &clientoor.Completed{}, stateMsg.State)
}

// TestOORServerRejectsTamperedFinalizeSignature ensures finalize fails
// if the client tampers with checkpoint signature material after
// co-signing.
func TestOORServerRejectsTamperedFinalizeSignature(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	sqlStore := db.NewTestDB(t)
	deliveryStore, err := db.NewActorDeliveryStoreFromDB(
		sqlStore, clock.NewDefaultClock(), btclog.Disabled,
	)
	require.NoError(t, err)

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	operatorSigner := input.NewMockSigner(
		[]*btcec.PrivateKey{operatorKey}, nil,
	)

	policy := scripts.CheckpointPolicy{
		OperatorKey: operatorKey.PubKey(),
		CSVDelay:    10,
	}

	driver := serveroor.NewDriver(serveroor.DriverCfg{
		OperatorSigner: operatorSigner,
		OperatorKey: keychain.KeyDescriptor{
			PubKey: operatorKey.PubKey(),
		},
	})

	server := startE2EServerActor(t, serveroor.ActorCfg{
		OutboxHandler:    driver,
		CheckpointPolicy: policy,
		DeliveryStore:    deliveryStore,
	})

	inputValue := btcutil.Amount(10000)
	exitDelay := uint32(10)

	senderKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	clientSigner := input.NewMockSigner(
		[]*btcec.PrivateKey{senderKey}, nil,
	)

	senderOutpoint := wire.OutPoint{
		Hash:  [32]byte{0x03},
		Index: 0,
	}

	inputs := []clientoor.TransferInput{
		newClientTransferInput(
			t, senderKey, policy.OperatorKey, exitDelay,
			senderOutpoint, inputValue,
		),
	}

	recipientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	recipientTapKey, err := scripts.VTXOTapKey(
		recipientKey.PubKey(), policy.OperatorKey, exitDelay,
	)
	require.NoError(t, err)

	recipientPkScript, err := txscript.PayToTaprootScript(
		recipientTapKey,
	)
	require.NoError(t, err)

	checkpointRes, err := oortx.BuildCheckpointPSBT(
		policy, oortx.CheckpointInput{
			SpentVTXO: oortx.SpentVTXORef{
				Outpoint: senderOutpoint,
				Output: &wire.TxOut{
					Value:    int64(inputValue),
					PkScript: inputs[0].VTXO.PkScript,
				},
			},
			OwnerLeafScript: inputs[0].OwnerLeafScript,
		},
	)
	require.NoError(t, err)

	arkPSBT, err := oortx.BuildArkPSBT(
		[]oortx.CheckpointOutput{{
			Txid: checkpointRes.PSBT.
				UnsignedTx.TxHash(),
			Output: checkpointRes.PSBT.
				UnsignedTx.TxOut[0],
			TapTreeEncoded: checkpointRes.TapTreeEncoded,
		}}, []oortx.RecipientOutput{{
			PkScript: recipientPkScript,
			Value:    inputValue,
		}},
	)
	require.NoError(t, err)

	leaf, err := oortx.BuildTaprootTapLeafScript(
		checkpointRes.TapTreeEncoded,
		inputs[0].OwnerLeafScript,
	)
	require.NoError(t, err)
	arkPSBT.Inputs[0].TaprootLeafScript =
		[]*psbt.TaprootTapLeafScript{leaf}

	submitResp := server.Receive(
		ctx, &serveroor.SubmitOORRequest{
			ArkPSBT: arkPSBT,
			CheckpointPSBTs: []*psbt.Packet{
				checkpointRes.PSBT,
			},
			VTXOSigningDescriptors: []serveroor.
				VTXOSigningDescriptor{{
				Outpoint:  senderOutpoint,
				OwnerKey:  senderKey.PubKey(),
				ExitDelay: exitDelay,
			}},
		},
	)
	require.True(t, submitResp.IsOk(), submitResp.Err())

	submitMsg, ok := submitResp.UnwrapOr(
		nil,
	).(*serveroor.SubmitOORResponse)
	require.True(t, ok)

	finalized := finalizeCheckpointPSBTsForTest(
		t, clientSigner, inputs, clonePSBTSliceForTest(
			t, submitMsg.CoSignedCheckpointPSBTs,
		),
	)
	tampered := clonePSBTSliceForTest(t, finalized)
	require.NotEmpty(t, tampered)
	require.NotEmpty(t, tampered[0].Inputs)
	require.NotEmpty(
		t, tampered[0].Inputs[0].TaprootScriptSpendSig,
	)

	operatorXOnly := schnorr.SerializePubKey(
		policy.OperatorKey,
	)

	var tamperedOwner bool
	for i := range tampered[0].Inputs[0].TaprootScriptSpendSig {
		sigRec := tampered[0].Inputs[0].
			TaprootScriptSpendSig[i]

		if sigRec == nil {
			continue
		}

		// Tamper the non-operator signature while preserving
		// operator signature bytes as sent in
		// submit-accepted.
		if bytes.Equal(sigRec.XOnlyPubKey, operatorXOnly) {
			continue
		}

		require.NotEmpty(t, sigRec.Signature)
		sigRec.Signature[0] ^= 0x01
		tamperedOwner = true

		break
	}
	require.True(t, tamperedOwner)

	finalizeResp := server.Receive(
		ctx, &serveroor.FinalizeOORRequest{
			SessionID:            submitMsg.SessionID,
			FinalCheckpointPSBTs: tampered,
		},
	)
	require.True(t, finalizeResp.IsErr())
	require.ErrorContains(
		t, finalizeResp.Err(), "owner signature invalid",
	)
}
