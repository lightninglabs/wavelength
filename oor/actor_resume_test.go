package oor

import (
	"context"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/scripts"
	oortx "github.com/lightninglabs/darepo-client/lib/tx/oor"
	"github.com/lightninglabs/darepo-client/lib/tx/psbtutil"
	"github.com/lightningnetwork/lnd/input"
	"github.com/stretchr/testify/require"
)

// pausedFinalizeHandler simulates a transport that drops the finalize response
// the first time finalize is sent, requiring an explicit resume/retry.
type pausedFinalizeHandler struct {
	t *testing.T

	clientSigner   input.Signer
	operatorSigner input.Signer

	finalizePaused bool
}

// Handle processes the outbox request and returns follow-up events.
func (h *pausedFinalizeHandler) Handle(_ context.Context, sessionID SessionID,
	outbox OutboxEvent) ([]Event, error) {

	h.t.Helper()

	switch msg := outbox.(type) {
	case *RequestArkSignatures:
		return []Event{
			&ArkSignedEvent{
				ArkPSBT: msg.ArkPSBT,
			},
		}, nil

	case *SendSubmitPackageRequest:
		txid := msg.ArkPSBT.UnsignedTx.TxHash()
		require.Equal(h.t, SessionID(txid), sessionID)

		err := coSignCheckpointPSBTsForTest(
			h.operatorSigner,
			msg.TransferInputs,
			msg.CheckpointPSBTs,
		)
		require.NoError(h.t, err)

		return []Event{
			&SubmitAcceptedEvent{
				SessionID:               sessionID,
				ArkPSBT:                 msg.ArkPSBT,
				CoSignedCheckpointPSBTs: msg.CheckpointPSBTs,
			},
		}, nil

	case *RequestCheckpointSignatures:
		err := SignCheckpointPSBTs(
			h.clientSigner, msg.TransferInputs,
			msg.CoSignedCheckpointPSBTs,
		)
		require.NoError(h.t, err)

		return []Event{
			&CheckpointsSignedEvent{
				FinalCheckpointPSBTs: msg.
					CoSignedCheckpointPSBTs,
			},
		}, nil

	case *SendFinalizePackageRequest:
		if !h.finalizePaused {
			h.finalizePaused = true
			return nil, nil
		}

		return []Event{
			&FinalizeAcceptedEvent{},
		}, nil

	case *MarkInputsSpentRequest:
		_ = msg
		return []Event{&InputsMarkedSpentEvent{}}, nil

	default:
		return nil, nil
	}
}

var _ OutboxHandler = (*pausedFinalizeHandler)(nil)

// pausedSubmitHandler simulates a transport that drops the submit response the
// first time submit is sent, requiring an explicit resume/retry.
type pausedSubmitHandler struct {
	t *testing.T

	clientSigner   input.Signer
	operatorSigner input.Signer

	submitPaused bool
}

// Handle processes the outbox request and returns follow-up events.
func (h *pausedSubmitHandler) Handle(_ context.Context, sessionID SessionID,
	outbox OutboxEvent) ([]Event, error) {

	h.t.Helper()

	switch msg := outbox.(type) {
	case *RequestArkSignatures:
		return []Event{
			&ArkSignedEvent{
				ArkPSBT: msg.ArkPSBT,
			},
		}, nil

	case *SendSubmitPackageRequest:
		txid := msg.ArkPSBT.UnsignedTx.TxHash()
		require.Equal(h.t, SessionID(txid), sessionID)

		err := coSignCheckpointPSBTsForTest(
			h.operatorSigner,
			msg.TransferInputs,
			msg.CheckpointPSBTs,
		)
		require.NoError(h.t, err)

		if !h.submitPaused {
			h.submitPaused = true
			return nil, nil
		}

		return []Event{
			&SubmitAcceptedEvent{
				SessionID:               sessionID,
				ArkPSBT:                 msg.ArkPSBT,
				CoSignedCheckpointPSBTs: msg.CheckpointPSBTs,
			},
		}, nil

	case *RequestCheckpointSignatures:
		err := SignCheckpointPSBTs(
			h.clientSigner, msg.TransferInputs,
			msg.CoSignedCheckpointPSBTs,
		)
		require.NoError(h.t, err)

		return []Event{
			&CheckpointsSignedEvent{
				FinalCheckpointPSBTs: msg.
					CoSignedCheckpointPSBTs,
			},
		}, nil

	case *SendFinalizePackageRequest:
		_ = msg

		return []Event{
			&FinalizeAcceptedEvent{},
		}, nil

	case *MarkInputsSpentRequest:
		_ = msg
		return []Event{&InputsMarkedSpentEvent{}}, nil

	default:
		return nil, nil
	}
}

var _ OutboxHandler = (*pausedSubmitHandler)(nil)

// pausedCoSignedHandler simulates a wallet/UI environment where checkpoint
// signing is not completed the first time it is requested (for example, the
// app is backgrounded), requiring resume.
type pausedCoSignedHandler struct {
	t *testing.T

	clientSigner   input.Signer
	operatorSigner input.Signer

	signPaused bool
}

// Handle processes the outbox request and returns follow-up events.
func (h *pausedCoSignedHandler) Handle(_ context.Context, sessionID SessionID,
	outbox OutboxEvent) ([]Event, error) {

	h.t.Helper()

	switch msg := outbox.(type) {
	case *RequestArkSignatures:
		return []Event{
			&ArkSignedEvent{
				ArkPSBT: msg.ArkPSBT,
			},
		}, nil

	case *SendSubmitPackageRequest:
		txid := msg.ArkPSBT.UnsignedTx.TxHash()
		require.Equal(h.t, SessionID(txid), sessionID)

		err := coSignCheckpointPSBTsForTest(
			h.operatorSigner,
			msg.TransferInputs,
			msg.CheckpointPSBTs,
		)
		require.NoError(h.t, err)

		return []Event{
			&SubmitAcceptedEvent{
				SessionID:               sessionID,
				ArkPSBT:                 msg.ArkPSBT,
				CoSignedCheckpointPSBTs: msg.CheckpointPSBTs,
			},
		}, nil

	case *RequestCheckpointSignatures:
		if !h.signPaused {
			h.signPaused = true
			return nil, nil
		}

		err := SignCheckpointPSBTs(
			h.clientSigner, msg.TransferInputs,
			msg.CoSignedCheckpointPSBTs,
		)
		require.NoError(h.t, err)

		return []Event{
			&CheckpointsSignedEvent{
				FinalCheckpointPSBTs: msg.
					CoSignedCheckpointPSBTs,
			},
		}, nil

	case *SendFinalizePackageRequest:
		_ = msg

		return []Event{
			&FinalizeAcceptedEvent{},
		}, nil

	case *MarkInputsSpentRequest:
		_ = msg
		return []Event{&InputsMarkedSpentEvent{}}, nil

	default:
		return nil, nil
	}
}

var _ OutboxHandler = (*pausedCoSignedHandler)(nil)

// cosignedButDroppedHandler simulates the point-of-no-return edge case:
// the server accepted and co-signed the submit package, but the client did not
// receive the SubmitAccepted response.
//
// On retry, the client must re-send the exact same submit package, and the
// server must return the original co-signed artifacts (not new ones).
type cosignedButDroppedHandler struct {
	t *testing.T

	clientSigner   input.Signer
	operatorSigner input.Signer

	firstSubmitDropped bool

	firstArkRaw         []byte
	firstCheckpointRaws [][]byte

	cosignedCheckpoints []*psbt.Packet
}

// Handle processes the outbox request and returns follow-up events.
func (h *cosignedButDroppedHandler) Handle(_ context.Context,
	sessionID SessionID, outbox OutboxEvent) ([]Event, error) {

	h.t.Helper()

	switch msg := outbox.(type) {
	case *RequestArkSignatures:
		return []Event{
			&ArkSignedEvent{
				ArkPSBT: msg.ArkPSBT,
			},
		}, nil

	case *SendSubmitPackageRequest:
		txid := msg.ArkPSBT.UnsignedTx.TxHash()
		require.Equal(h.t, SessionID(txid), sessionID)

		err := coSignCheckpointPSBTsForTest(
			h.operatorSigner,
			msg.TransferInputs,
			msg.CheckpointPSBTs,
		)
		require.NoError(h.t, err)

		arkRaw, err := psbtutil.Serialize(msg.ArkPSBT)
		require.NoError(h.t, err)

		cpRaws, err := serializePSBTSlice(msg.CheckpointPSBTs)
		require.NoError(h.t, err)

		// First submit: simulate server co-signing and persisting, but
		// drop the response back to the client.
		if !h.firstSubmitDropped {
			h.firstSubmitDropped = true
			h.firstArkRaw = arkRaw
			h.firstCheckpointRaws = cpRaws

			// In v0 tests we don't need a real operator signature.
			// We only need stable "co-signed" artifacts for the
			// client to resume with.
			//
			// We model this by deep-copying the checkpoint PSBTs.
			// Then we hold them for the eventual retry response.
			h.cosignedCheckpoints, err = parsePSBTSlice(cpRaws)
			require.NoError(h.t, err)

			return nil, nil
		}

		// Retry: client must resend the exact same package.
		require.Equal(h.t, h.firstArkRaw, arkRaw,
			"ark psbt differs across submit retries")
		require.Equal(h.t, h.firstCheckpointRaws, cpRaws,
			"checkpoint psbts differ across submit retries")

		return []Event{
			&SubmitAcceptedEvent{
				SessionID: sessionID,
				ArkPSBT:   msg.ArkPSBT,
				CoSignedCheckpointPSBTs: h.
					cosignedCheckpoints,
			},
		}, nil

	case *RequestCheckpointSignatures:
		err := SignCheckpointPSBTs(
			h.clientSigner, msg.TransferInputs,
			msg.CoSignedCheckpointPSBTs,
		)
		require.NoError(h.t, err)

		return []Event{
			&CheckpointsSignedEvent{
				FinalCheckpointPSBTs: msg.
					CoSignedCheckpointPSBTs,
			},
		}, nil

	case *SendFinalizePackageRequest:
		_ = msg

		return []Event{
			&FinalizeAcceptedEvent{},
		}, nil

	case *MarkInputsSpentRequest:
		_ = msg
		return []Event{&InputsMarkedSpentEvent{}}, nil

	default:
		return nil, nil
	}
}

var _ OutboxHandler = (*cosignedButDroppedHandler)(nil)

// TestOORClientActorResumeFromSnapshot verifies the client actor can export a
// snapshot, restore it into a new actor, and resume the workflow to completion.
func TestOORClientActorResumeFromSnapshot(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	// Build a deterministic transfer with a mocked client signer. The
	// outbox handler will pause on finalize to simulate a transport/UI
	// interruption that requires an explicit resume.
	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	policy := scripts.CheckpointPolicy{
		OperatorKey: operatorKey.PubKey(),
		CSVDelay:    10,
	}

	inputValue := btcutil.Amount(10000)

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	clientSigner := input.NewMockSigner([]*btcec.PrivateKey{clientKey}, nil)
	operatorSigner := input.NewMockSigner(
		[]*btcec.PrivateKey{operatorKey}, nil,
	)

	inputs := []TransferInput{
		newTestTransferInput(
			t, clientKey, policy.OperatorKey,
			wire.OutPoint{
				Hash:  [32]byte{0x01},
				Index: 0,
			},
			inputValue,
		),
	}

	recipients := []oortx.RecipientOutput{
		{
			PkScript: newTestTaprootPkScript(t, clientKey.PubKey()),
			Value:    inputValue,
		},
	}

	deliveryStore := newTestDeliveryStore(t)

	handler := &pausedFinalizeHandler{
		t:              t,
		clientSigner:   clientSigner,
		operatorSigner: operatorSigner,
	}

	// Start a session and drive it until finalize is sent (but "dropped"
	// by the outbox handler).
	actor1 := NewOORClientActor(ClientActorCfg{
		OutboxHandler: handler,
		DeliveryStore: deliveryStore,
		ActorID:       "oor-resume-snapshot-actor-1",
	})
	defer actor1.Stop()

	startResp := actor1.Receive(ctx, &StartTransferRequest{
		Policy:     policy,
		Inputs:     inputs,
		Recipients: recipients,
	})
	require.True(t, startResp.IsOk())

	startMsg, ok := startResp.UnwrapOr(nil).(*StartTransferResponse)
	require.True(t, ok)

	stateResp := actor1.Receive(ctx, &GetStateRequest{
		SessionID: startMsg.SessionID,
	})
	require.True(t, stateResp.IsOk())

	stateMsg, ok := stateResp.UnwrapOr(nil).(*GetStateResponse)
	require.True(t, ok)

	require.IsType(t, &AwaitingFinalizeAccepted{}, stateMsg.State)

	// Export a portable snapshot, then create a new actor and restore from
	// it to simulate an app restart.
	//
	// The key property is that the snapshot contains enough information to
	// re-emit the outbox work implied by the state (idempotently).
	exportResp := actor1.Receive(ctx, &ExportSnapshotRequest{
		SessionID: startMsg.SessionID,
	})
	require.True(t, exportResp.IsOk())

	exportMsg, ok := exportResp.UnwrapOr(nil).(*ExportSnapshotResponse)
	require.True(t, ok)
	require.NotNil(t, exportMsg.Snapshot)
	require.Equal(t, OutgoingPhaseFinalizeSent, exportMsg.Snapshot.Phase)

	// Restore into a new actor and resume.
	actor2 := NewOORClientActor(ClientActorCfg{
		OutboxHandler: handler,
		DeliveryStore: deliveryStore,
		ActorID:       "oor-resume-snapshot-actor-2",
	})
	defer actor2.Stop()

	restoreResp := actor2.Receive(ctx, &RestoreSessionRequest{
		Snapshot: exportMsg.Snapshot,
	})
	require.True(t, restoreResp.IsOk())

	restoreMsg, ok := restoreResp.UnwrapOr(nil).(*RestoreSessionResponse)
	require.True(t, ok)
	require.Equal(t, startMsg.SessionID, restoreMsg.SessionID)

	resumeResp := actor2.Receive(ctx, &ResumeSessionRequest{
		SessionID: restoreMsg.SessionID,
	})
	require.True(t, resumeResp.IsOk())

	finalStateResp := actor2.Receive(ctx, &GetStateRequest{
		SessionID: restoreMsg.SessionID,
	})
	require.True(t, finalStateResp.IsOk())

	finalStateMsg, ok := finalStateResp.UnwrapOr(nil).(*GetStateResponse)
	require.True(t, ok)
	require.IsType(t, &Completed{}, finalStateMsg.State)
}

// TestOORClientActorResumeAfterServerCoSigned verifies the client can resume
// safely if the server reached point-of-no-return (co-signed) but the client
// missed the submit response.
func TestOORClientActorResumeAfterServerCoSigned(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	// This test covers the "point of no return" edge:
	//
	// - The server has accepted the submit package and co-signed it.
	// - The client did not receive SubmitAcceptedEvent.
	//
	// On resume, the client must send the exact same submit bytes and the
	// server must return the same co-signed artifacts.
	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	policy := scripts.CheckpointPolicy{
		OperatorKey: operatorKey.PubKey(),
		CSVDelay:    10,
	}

	inputValue := btcutil.Amount(10000)

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	clientSigner := input.NewMockSigner([]*btcec.PrivateKey{clientKey}, nil)
	operatorSigner := input.NewMockSigner(
		[]*btcec.PrivateKey{operatorKey}, nil,
	)

	inputs := []TransferInput{
		newTestTransferInput(
			t, clientKey, policy.OperatorKey,
			wire.OutPoint{
				Hash:  [32]byte{0x01},
				Index: 0,
			},
			inputValue,
		),
	}

	recipients := []oortx.RecipientOutput{
		{
			PkScript: newTestTaprootPkScript(t, clientKey.PubKey()),
			Value:    inputValue,
		},
	}

	deliveryStore := newTestDeliveryStore(t)
	handler := &cosignedButDroppedHandler{
		t:              t,
		clientSigner:   clientSigner,
		operatorSigner: operatorSigner,
	}

	actor1 := NewOORClientActor(ClientActorCfg{
		OutboxHandler: handler,
		DeliveryStore: deliveryStore,
		ActorID:       "oor-resume-cosigned-actor-1",
	})
	defer actor1.Stop()

	startResp := actor1.Receive(ctx, &StartTransferRequest{
		Policy:     policy,
		Inputs:     inputs,
		Recipients: recipients,
	})
	require.True(t, startResp.IsOk())

	startMsg, ok := startResp.UnwrapOr(nil).(*StartTransferResponse)
	require.True(t, ok)

	// At this point, the handler simulated the server already co-signing
	// but the client did not receive the response, so we should still be
	// waiting for submit acceptance.
	stateResp := actor1.Receive(ctx, &GetStateRequest{
		SessionID: startMsg.SessionID,
	})
	require.True(t, stateResp.IsOk())

	stateMsg, ok := stateResp.UnwrapOr(nil).(*GetStateResponse)
	require.True(t, ok)
	require.IsType(t, &AwaitingSubmitAccepted{}, stateMsg.State)

	exportResp := actor1.Receive(ctx, &ExportSnapshotRequest{
		SessionID: startMsg.SessionID,
	})
	require.True(t, exportResp.IsOk())

	exportMsg, ok := exportResp.UnwrapOr(nil).(*ExportSnapshotResponse)
	require.True(t, ok)
	require.NotNil(t, exportMsg.Snapshot)
	require.Equal(t, OutgoingPhaseSubmitSent, exportMsg.Snapshot.Phase)

	// Restore into a new actor and resume (which should re-send submit and
	// receive the already-co-signed artifacts).
	actor2 := NewOORClientActor(ClientActorCfg{
		OutboxHandler: handler,
		DeliveryStore: deliveryStore,
		ActorID:       "oor-resume-cosigned-actor-2",
	})
	defer actor2.Stop()

	restoreResp := actor2.Receive(ctx, &RestoreSessionRequest{
		Snapshot: exportMsg.Snapshot,
	})
	require.True(t, restoreResp.IsOk())

	restoreMsg, ok := restoreResp.UnwrapOr(nil).(*RestoreSessionResponse)
	require.True(t, ok)

	resumeResp := actor2.Receive(ctx, &ResumeSessionRequest{
		SessionID: restoreMsg.SessionID,
	})
	require.True(t, resumeResp.IsOk())

	finalStateResp := actor2.Receive(ctx, &GetStateRequest{
		SessionID: restoreMsg.SessionID,
	})
	require.True(t, finalStateResp.IsOk())

	finalStateMsg, ok := finalStateResp.UnwrapOr(nil).(*GetStateResponse)
	require.True(t, ok)
	require.IsType(t, &Completed{}, finalStateMsg.State)
}

// TestOORClientActorResumeAfterServerCoSignedFromStore verifies the client can
// resume after a crash using only the persisted snapshot (no explicit export)
// even if the server already co-signed but the submit response was dropped.
func TestOORClientActorResumeAfterServerCoSignedFromStore(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	policy := scripts.CheckpointPolicy{
		OperatorKey: operatorKey.PubKey(),
		CSVDelay:    10,
	}

	inputValue := btcutil.Amount(10000)

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	clientSigner := input.NewMockSigner([]*btcec.PrivateKey{clientKey}, nil)

	inputs := []TransferInput{
		newTestTransferInput(
			t, clientKey, policy.OperatorKey,
			wire.OutPoint{
				Hash:  [32]byte{0x01},
				Index: 0,
			},
			inputValue,
		),
	}

	recipients := []oortx.RecipientOutput{
		{
			PkScript: newTestTaprootPkScript(t, clientKey.PubKey()),
			Value:    inputValue,
		},
	}

	deliveryStore := newTestDeliveryStore(t)
	handler := &cosignedButDroppedHandler{
		t:            t,
		clientSigner: clientSigner,
	}

	const actorID = "oor-resume-cosigned-from-store-actor"

	actor1 := NewOORClientActor(ClientActorCfg{
		OutboxHandler: handler,
		DeliveryStore: deliveryStore,
		ActorID:       actorID,
	})
	defer actor1.Stop()

	startResp := actor1.Receive(ctx, &StartTransferRequest{
		Policy:     policy,
		Inputs:     inputs,
		Recipients: recipients,
	})
	require.True(t, startResp.IsOk())

	startMsg, ok := startResp.UnwrapOr(nil).(*StartTransferResponse)
	require.True(t, ok)

	// At this point the server has co-signed but the response was dropped,
	// so the client should still be waiting for submit acceptance.
	stateResp := actor1.Receive(ctx, &GetStateRequest{
		SessionID: startMsg.SessionID,
	})
	require.True(t, stateResp.IsOk())

	stateMsg, ok := stateResp.UnwrapOr(nil).(*GetStateResponse)
	require.True(t, ok)
	require.IsType(t, &AwaitingSubmitAccepted{}, stateMsg.State)

	actor1.Stop()

	actor2 := NewOORClientActor(ClientActorCfg{
		OutboxHandler: handler,
		DeliveryStore: deliveryStore,
		ActorID:       actorID,
	})
	defer actor2.Stop()

	require.Eventually(t, func() bool {
		finalStateResp := actor2.Receive(ctx, &GetStateRequest{
			SessionID: startMsg.SessionID,
		})
		if finalStateResp.IsErr() {
			return false
		}

		respMsg := finalStateResp.UnwrapOr(nil)
		finalStateMsg, ok := respMsg.(*GetStateResponse)
		if !ok {
			return false
		}

		_, ok = finalStateMsg.State.(*Completed)

		return ok
	}, 5*time.Second, 50*time.Millisecond)
}

// TestOORClientActorResumeFromSnapshotSubmitSent verifies the client can resume
// after submit was sent but the response was dropped.
func TestOORClientActorResumeFromSnapshotSubmitSent(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	policy := scripts.CheckpointPolicy{
		OperatorKey: operatorKey.PubKey(),
		CSVDelay:    10,
	}

	inputValue := btcutil.Amount(10000)

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	clientSigner := input.NewMockSigner([]*btcec.PrivateKey{clientKey}, nil)
	operatorSigner := input.NewMockSigner(
		[]*btcec.PrivateKey{operatorKey}, nil,
	)

	inputs := []TransferInput{
		newTestTransferInput(
			t, clientKey, policy.OperatorKey,
			wire.OutPoint{
				Hash:  [32]byte{0x01},
				Index: 0,
			},
			inputValue,
		),
	}

	recipients := []oortx.RecipientOutput{
		{
			PkScript: newTestTaprootPkScript(t, clientKey.PubKey()),
			Value:    inputValue,
		},
	}

	deliveryStore := newTestDeliveryStore(t)
	handler := &pausedSubmitHandler{
		t:              t,
		clientSigner:   clientSigner,
		operatorSigner: operatorSigner,
	}

	actor1 := NewOORClientActor(ClientActorCfg{
		OutboxHandler: handler,
		DeliveryStore: deliveryStore,
		ActorID:       "oor-resume-submit-actor-1",
	})
	defer actor1.Stop()

	startResp := actor1.Receive(ctx, &StartTransferRequest{
		Policy:     policy,
		Inputs:     inputs,
		Recipients: recipients,
	})
	require.True(t, startResp.IsOk())

	startMsg, ok := startResp.UnwrapOr(nil).(*StartTransferResponse)
	require.True(t, ok)

	stateResp := actor1.Receive(ctx, &GetStateRequest{
		SessionID: startMsg.SessionID,
	})
	require.True(t, stateResp.IsOk())

	stateMsg, ok := stateResp.UnwrapOr(nil).(*GetStateResponse)
	require.True(t, ok)
	require.IsType(t, &AwaitingSubmitAccepted{}, stateMsg.State)

	exportResp := actor1.Receive(ctx, &ExportSnapshotRequest{
		SessionID: startMsg.SessionID,
	})
	require.True(t, exportResp.IsOk())

	exportMsg, ok := exportResp.UnwrapOr(nil).(*ExportSnapshotResponse)
	require.True(t, ok)
	require.NotNil(t, exportMsg.Snapshot)
	require.Equal(t, OutgoingPhaseSubmitSent, exportMsg.Snapshot.Phase)

	actor2 := NewOORClientActor(ClientActorCfg{
		OutboxHandler: handler,
		DeliveryStore: deliveryStore,
		ActorID:       "oor-resume-submit-actor-2",
	})
	defer actor2.Stop()

	restoreResp := actor2.Receive(ctx, &RestoreSessionRequest{
		Snapshot: exportMsg.Snapshot,
	})
	require.True(t, restoreResp.IsOk())

	restoreMsg, ok := restoreResp.UnwrapOr(nil).(*RestoreSessionResponse)
	require.True(t, ok)

	resumeResp := actor2.Receive(ctx, &ResumeSessionRequest{
		SessionID: restoreMsg.SessionID,
	})
	require.True(t, resumeResp.IsOk())

	finalStateResp := actor2.Receive(ctx, &GetStateRequest{
		SessionID: restoreMsg.SessionID,
	})
	require.True(t, finalStateResp.IsOk())

	finalStateMsg, ok := finalStateResp.UnwrapOr(nil).(*GetStateResponse)
	require.True(t, ok)
	require.IsType(t, &Completed{}, finalStateMsg.State)
}

// TestOORClientActorResumeFromSnapshotCoSigned verifies the client can resume
// after the server accepted/co-signed but the client did not complete signing
// checkpoints yet.
func TestOORClientActorResumeFromSnapshotCoSigned(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	policy := scripts.CheckpointPolicy{
		OperatorKey: operatorKey.PubKey(),
		CSVDelay:    10,
	}

	inputValue := btcutil.Amount(10000)

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	clientSigner := input.NewMockSigner([]*btcec.PrivateKey{clientKey}, nil)
	operatorSigner := input.NewMockSigner(
		[]*btcec.PrivateKey{operatorKey}, nil,
	)

	inputs := []TransferInput{
		newTestTransferInput(
			t, clientKey, policy.OperatorKey,
			wire.OutPoint{
				Hash:  [32]byte{0x01},
				Index: 0,
			},
			inputValue,
		),
	}

	recipients := []oortx.RecipientOutput{
		{
			PkScript: newTestTaprootPkScript(t, clientKey.PubKey()),
			Value:    inputValue,
		},
	}

	deliveryStore := newTestDeliveryStore(t)
	handler := &pausedCoSignedHandler{
		t:              t,
		clientSigner:   clientSigner,
		operatorSigner: operatorSigner,
	}

	actor1 := NewOORClientActor(ClientActorCfg{
		OutboxHandler: handler,
		DeliveryStore: deliveryStore,
		ActorID:       "oor-resume-cosigned-phase-actor-1",
	})
	defer actor1.Stop()

	startResp := actor1.Receive(ctx, &StartTransferRequest{
		Policy:     policy,
		Inputs:     inputs,
		Recipients: recipients,
	})
	require.True(t, startResp.IsOk())

	startMsg, ok := startResp.UnwrapOr(nil).(*StartTransferResponse)
	require.True(t, ok)

	stateResp := actor1.Receive(ctx, &GetStateRequest{
		SessionID: startMsg.SessionID,
	})
	require.True(t, stateResp.IsOk())

	stateMsg, ok := stateResp.UnwrapOr(nil).(*GetStateResponse)
	require.True(t, ok)
	require.IsType(t, &AwaitingCheckpointSignatures{}, stateMsg.State)

	exportResp := actor1.Receive(ctx, &ExportSnapshotRequest{
		SessionID: startMsg.SessionID,
	})
	require.True(t, exportResp.IsOk())

	exportMsg, ok := exportResp.UnwrapOr(nil).(*ExportSnapshotResponse)
	require.True(t, ok)
	require.NotNil(t, exportMsg.Snapshot)
	require.Equal(t, OutgoingPhaseCoSigned, exportMsg.Snapshot.Phase)

	actor2 := NewOORClientActor(ClientActorCfg{
		OutboxHandler: handler,
		DeliveryStore: deliveryStore,
		ActorID:       "oor-resume-cosigned-phase-actor-2",
	})
	defer actor2.Stop()

	restoreResp := actor2.Receive(ctx, &RestoreSessionRequest{
		Snapshot: exportMsg.Snapshot,
	})
	require.True(t, restoreResp.IsOk())

	restoreMsg, ok := restoreResp.UnwrapOr(nil).(*RestoreSessionResponse)
	require.True(t, ok)

	resumeResp := actor2.Receive(ctx, &ResumeSessionRequest{
		SessionID: restoreMsg.SessionID,
	})
	require.True(t, resumeResp.IsOk())

	finalStateResp := actor2.Receive(ctx, &GetStateRequest{
		SessionID: restoreMsg.SessionID,
	})
	require.True(t, finalStateResp.IsOk())

	finalStateMsg, ok := finalStateResp.UnwrapOr(nil).(*GetStateResponse)
	require.True(t, ok)
	require.IsType(t, &Completed{}, finalStateMsg.State)
}

// TestOORClientActorDurableRestartAutoResume verifies the durable actor can
// restore checkpointed sessions and auto-resume pending outbox work after a
// process restart, without using ExportSnapshot/RestoreSession requests.
func TestOORClientActorDurableRestartAutoResume(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	policy := scripts.CheckpointPolicy{
		OperatorKey: operatorKey.PubKey(),
		CSVDelay:    10,
	}

	inputValue := btcutil.Amount(10000)

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	clientSigner := input.NewMockSigner([]*btcec.PrivateKey{clientKey}, nil)
	operatorSigner := input.NewMockSigner(
		[]*btcec.PrivateKey{operatorKey}, nil,
	)

	inputs := []TransferInput{
		newTestTransferInput(
			t, clientKey, policy.OperatorKey,
			wire.OutPoint{
				Hash:  [32]byte{0x01},
				Index: 0,
			},
			inputValue,
		),
	}

	recipients := []oortx.RecipientOutput{
		{
			PkScript: newTestTaprootPkScript(t, clientKey.PubKey()),
			Value:    inputValue,
		},
	}

	deliveryStore := newTestDeliveryStore(t)
	handler := &pausedFinalizeHandler{
		t:              t,
		clientSigner:   clientSigner,
		operatorSigner: operatorSigner,
	}

	const actorID = "oor-durable-restart-actor"

	actor1 := NewOORClientActor(ClientActorCfg{
		OutboxHandler: handler,
		DeliveryStore: deliveryStore,
		ActorID:       actorID,
	})

	startResp := actor1.Receive(ctx, &StartTransferRequest{
		Policy:     policy,
		Inputs:     inputs,
		Recipients: recipients,
	})
	require.True(t, startResp.IsOk())

	startMsg, ok := startResp.UnwrapOr(nil).(*StartTransferResponse)
	require.True(t, ok)

	stateResp := actor1.Receive(ctx, &GetStateRequest{
		SessionID: startMsg.SessionID,
	})
	require.True(t, stateResp.IsOk())

	stateMsg, ok := stateResp.UnwrapOr(nil).(*GetStateResponse)
	require.True(t, ok)
	require.IsType(t, &AwaitingFinalizeAccepted{}, stateMsg.State)

	actor1.Stop()

	actor2 := NewOORClientActor(ClientActorCfg{
		OutboxHandler: handler,
		DeliveryStore: deliveryStore,
		ActorID:       actorID,
	})
	defer actor2.Stop()

	require.Eventually(t, func() bool {
		resp := actor2.Receive(ctx, &GetStateRequest{
			SessionID: startMsg.SessionID,
		})
		if resp.IsErr() {
			return false
		}

		got, ok := resp.UnwrapOr(nil).(*GetStateResponse)
		if !ok {
			return false
		}

		switch got.State.(type) {
		case *AwaitingLocalVTXOUpdate, *Completed:
			return true
		default:
			return false
		}
	}, 5*time.Second, 50*time.Millisecond)
}
