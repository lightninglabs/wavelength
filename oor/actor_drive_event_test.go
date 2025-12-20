package oor

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/scripts"
	oortx "github.com/lightninglabs/darepo-client/lib/tx/oor"
	"github.com/stretchr/testify/require"
)

// TestOORClientActorDriveEventErrors asserts the DriveEventRequest handler
// rejects malformed inputs.
func TestOORClientActorDriveEventErrors(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	actor := NewOORClientActor(ClientActorCfg{})

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

// TestOORClientActorDriveEventHappyPath asserts the actor can drive an
// in-flight session forward via DriveEventRequest.
func TestOORClientActorDriveEventHappyPath(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	// Use a minimal v0 transfer to create a real session, then inject an
	// event as if it came from an outbox boundary.
	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	policy := scripts.CheckpointPolicy{
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

	actor := NewOORClientActor(ClientActorCfg{})

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

	// Drive the state machine forward by injecting a terminal failure
	// event. This is used by higher layers to handle out-of-band failures
	// (for example, the wallet UI rejecting a signing request).
	driveResp := actor.Receive(ctx, &DriveEventRequest{
		SessionID: startMsg.SessionID,
		Event:     &FailEvent{Reason: "boom"},
	})
	require.True(t, driveResp.IsOk())
	_, ok = driveResp.UnwrapOr(nil).(*DriveEventResponse)
	require.True(t, ok)

	// Verify the session state was updated.
	stateResp := actor.Receive(ctx, &GetStateRequest{
		SessionID: startMsg.SessionID,
	})
	require.True(t, stateResp.IsOk())

	stateMsg, ok := stateResp.UnwrapOr(nil).(*GetStateResponse)
	require.True(t, ok)
	require.IsType(t, &Failed{}, stateMsg.State)
}
