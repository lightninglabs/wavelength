package oor

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	oortx "github.com/lightninglabs/darepo-client/lib/tx/oor"
	"github.com/lightningnetwork/lnd/input"
	"github.com/stretchr/testify/require"
)

type noopSigningEffectRef struct{}

func (noopSigningEffectRef) ID() string {
	return "noop-signing-effect"
}

func (noopSigningEffectRef) Tell(_ context.Context, _ SigningEffectMsg) error {
	return nil
}

var _ actor.TellOnlyRef[SigningEffectMsg] = (*noopSigningEffectRef)(nil)

// TestOORClientActorDriveEventErrors asserts the DriveEventRequest handler
// rejects malformed inputs.
func TestOORClientActorDriveEventErrors(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	actor := NewOORClientActor(ClientActorCfg{
		DeliveryStore: newTestDeliveryStore(t),
		ActorID:       "oor-drive-event-errors",
	})
	defer actor.Stop()

	// DriveEventRequest is an escape hatch for delivering out-of-band
	// events into a running session.
	//
	// These error cases should be rejected early so higher layers don't
	// accidentally create sessions or mutate state.

	// Nil request is rejected.
	resp := actor.Receive(ctx, (*DriveEventRequest)(nil))
	require.True(t, resp.IsErr())

	// Missing event is rejected.
	resp = actor.Receive(ctx, &DriveEventRequest{
		SessionID: SessionID{},
		Event:     nil,
	})
	require.True(t, resp.IsErr())

	// Unknown session id is rejected.
	resp = actor.Receive(ctx, &DriveEventRequest{
		SessionID: SessionID{1},
		Event:     &FailEvent{Reason: "boom"},
	})
	require.True(t, resp.IsErr())
}

// TestOORClientActorDriveEventAppliesEvent asserts DriveEventRequest is
// accepted and routed into the running session FSM.
func TestOORClientActorDriveEventAppliesEvent(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	// Use a minimal v0 transfer to create a real session, then inject an
	// event as if it came from an outbox boundary.
	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	policy := arkscript.CheckpointPolicy{
		OperatorKey: operatorKey.PubKey(),
		CSVDelay:    10,
	}

	inputValue := btcutil.Amount(10_000)

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	inputs := []TransferInput{
		newTestTransferInput(
			t,
			clientKey,
			policy.OperatorKey,
			wire.OutPoint{
				Hash:  [32]byte{0x01},
				Index: 0,
			},
			inputValue,
		),
	}

	recipients := []oortx.RecipientOutput{{
		PkScript: newTestTaprootPkScript(t, clientKey.PubKey()),
		Value:    inputValue,
	}}

	actor := NewOORClientActor(ClientActorCfg{
		OutboxHandler: &noopOutboxHandler{},
		DeliveryStore: newTestDeliveryStore(t),
		ActorID:       "oor-drive-event-happy",
	})
	defer actor.Stop()

	// Start a session; this binds the session id to the Ark txid.
	startResp := actor.Receive(ctx, &StartTransferRequest{
		Policy:     policy,
		Inputs:     inputs,
		Recipients: recipients,
	})
	require.True(t, startResp.IsOk())

	startMsg, ok := startResp.UnwrapOr(nil).(*StartTransferResponse)
	require.True(t, ok)
	require.NotEqual(t, SessionID{}, startMsg.SessionID)

	// Drive a terminal event into the session and verify the FSM
	// transitions.
	driveResp := actor.Receive(ctx, &DriveEventRequest{
		SessionID: startMsg.SessionID,
		Event:     &FailEvent{Reason: "boom"},
	})
	require.True(t, driveResp.IsOk())

	// Verify the session moved into terminal failed state.
	stateResp := actor.Receive(ctx, &GetStateRequest{
		SessionID: startMsg.SessionID,
	})
	require.True(t, stateResp.IsOk())

	stateMsg, ok := stateResp.UnwrapOr(nil).(*GetStateResponse)
	require.True(t, ok)
	failedState, ok := stateMsg.State.(*Failed)
	require.True(t, ok)
	require.Equal(t, "boom", failedState.Reason)
}

// TestOORClientActorDriveEventIgnoresStaleSigningResults verifies durable
// signing effect replay can re-deliver a signing result after OOR has already
// advanced past that signing stage.
func TestOORClientActorDriveEventIgnoresStaleSigningResults(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	clientSigner := input.NewMockSigner([]*btcec.PrivateKey{clientKey}, nil)
	operatorSigner := input.NewMockSigner(
		[]*btcec.PrivateKey{operatorKey}, nil,
	)

	policy := arkscript.CheckpointPolicy{
		OperatorKey: operatorKey.PubKey(),
		CSVDelay:    10,
	}

	inputValue := btcutil.Amount(10_000)
	inputs := []TransferInput{
		newTestTransferInput(
			t,
			clientKey,
			policy.OperatorKey,
			wire.OutPoint{
				Hash:  [32]byte{0x02},
				Index: 0,
			},
			inputValue,
		),
	}
	recipients := []oortx.RecipientOutput{{
		PkScript: newTestTaprootPkScript(t, clientKey.PubKey()),
		Value:    inputValue,
	}}

	actor := NewOORClientActor(ClientActorCfg{
		OutboxHandler: &noopOutboxHandler{},
		DeliveryStore: newTestDeliveryStore(t),
		ActorID:       "oor-drive-event-stale-signing",
		SigningEffect: &noopSigningEffectRef{},
	})
	defer actor.Stop()

	startResp := actor.Receive(ctx, &StartTransferRequest{
		Policy:     policy,
		Inputs:     inputs,
		Recipients: recipients,
	})
	require.True(t, startResp.IsOk())

	startMsg, ok := startResp.UnwrapOr(nil).(*StartTransferResponse)
	require.True(t, ok)

	stateMsg := requireOORState[*AwaitingArkSignatures](
		t, ctx, actor, startMsg.SessionID,
	)

	err = SignArkPSBT(
		clientSigner, stateMsg.ArkPSBT, stateMsg.CheckpointPSBTs,
		stateMsg.TransferInputs,
	)
	require.NoError(t, err)

	arkSigned := &ArkSignedEvent{
		ArkPSBT: stateMsg.ArkPSBT,
	}
	driveResp := actor.Receive(ctx, &DriveEventRequest{
		SessionID: startMsg.SessionID,
		Event:     arkSigned,
	})
	require.True(t, driveResp.IsOk())
	requireOORState[*AwaitingSubmitAccepted](
		t, ctx, actor, startMsg.SessionID,
	)

	driveResp = actor.Receive(ctx, &DriveEventRequest{
		SessionID: startMsg.SessionID,
		Event:     arkSigned,
	})
	require.True(t, driveResp.IsOk())
	submitState := requireOORState[*AwaitingSubmitAccepted](
		t, ctx, actor, startMsg.SessionID,
	)

	err = coSignCheckpointPSBTsForTest(
		operatorSigner, submitState.TransferInputs,
		submitState.CheckpointPSBTs,
	)
	require.NoError(t, err)

	driveResp = actor.Receive(ctx, &DriveEventRequest{
		SessionID: startMsg.SessionID,
		Event: &SubmitAcceptedEvent{
			SessionID:               startMsg.SessionID,
			ArkPSBT:                 submitState.ArkPSBT,
			CoSignedCheckpointPSBTs: submitState.CheckpointPSBTs,
		},
	})
	require.True(t, driveResp.IsOk())

	checkpointState := requireOORState[*AwaitingCheckpointSignatures](
		t, ctx, actor, startMsg.SessionID,
	)
	err = SignCheckpointPSBTs(
		clientSigner, checkpointState.TransferInputs,
		checkpointState.CoSignedCheckpointPSBTs,
	)
	require.NoError(t, err)

	checkpointsSigned := &CheckpointsSignedEvent{
		FinalCheckpointPSBTs: checkpointState.CoSignedCheckpointPSBTs,
	}
	driveResp = actor.Receive(ctx, &DriveEventRequest{
		SessionID: startMsg.SessionID,
		Event:     checkpointsSigned,
	})
	require.True(t, driveResp.IsOk())
	requireOORState[*AwaitingFinalizeAccepted](
		t, ctx, actor, startMsg.SessionID,
	)

	driveResp = actor.Receive(ctx, &DriveEventRequest{
		SessionID: startMsg.SessionID,
		Event:     checkpointsSigned,
	})
	require.True(t, driveResp.IsOk())
	requireOORState[*AwaitingFinalizeAccepted](
		t, ctx, actor, startMsg.SessionID,
	)
}

func requireOORState[T State](t *testing.T, ctx context.Context,
	actor *OORClientActor, sessionID SessionID) T {

	t.Helper()

	stateResp := actor.Receive(ctx, &GetStateRequest{
		SessionID: sessionID,
	})
	require.True(t, stateResp.IsOk())

	stateMsg, ok := stateResp.UnwrapOr(nil).(*GetStateResponse)
	require.True(t, ok)

	state, ok := stateMsg.State.(T)
	require.True(t, ok)

	return state
}
