//nolint:ll
package oor

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	oortx "github.com/lightninglabs/darepo-client/lib/tx/oor"
	"github.com/lightningnetwork/lnd/input"
	"github.com/stretchr/testify/require"
)

func TestClientCoordinatorStartTransferAndRestart(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	operatorSigner := input.NewMockSigner(
		[]*btcec.PrivateKey{operatorKey}, nil,
	)
	clientSigner := input.NewMockSigner(
		[]*btcec.PrivateKey{clientKey}, nil,
	)

	const inputValue = btcutil.Amount(10_000)
	inputs := []TransferInput{
		newTestTransferInput(
			t, clientKey, operatorKey.PubKey(), wire.OutPoint{
				Hash:  [32]byte{0x01},
				Index: 0,
			}, inputValue,
		),
	}

	recipients := []oortx.RecipientOutput{{
		PkScript: newTestTaprootPkScript(t, clientKey.PubKey()),
		Value:    inputValue,
	}}

	sessionStore := newTestSessionStore()
	packageStore := &testPackageStore{}

	coord := NewClientCoordinator(ClientActorCfg{
		OutboxHandler: &testOutboxHandler{
			t:              t,
			clientSigner:   clientSigner,
			operatorSigner: operatorSigner,
		},
		PackageStore: packageStore,
		SessionStore: sessionStore,
		ActorID:      "oor-client-coordinator-test",
	})
	require.NoError(t, coord.Start(ctx))

	startResp := coord.Receive(ctx, &StartTransferRequest{
		Policy: arkscript.CheckpointPolicy{
			OperatorKey: operatorKey.PubKey(),
			CSVDelay:    10,
		},
		Inputs:         inputs,
		Recipients:     recipients,
		IdempotencyKey: "send-once",
	})
	require.True(t, startResp.IsOk())

	startMsg, ok := startResp.UnwrapOr(nil).(*StartTransferResponse)
	require.True(t, ok)
	require.False(t, startMsg.Existing)
	require.NotEqual(t, SessionID{}, startMsg.SessionID)

	stateResp := coord.Receive(ctx, &GetStateRequest{
		SessionID: startMsg.SessionID,
	})
	require.True(t, stateResp.IsOk())
	stateMsg, ok := stateResp.UnwrapOr(nil).(*GetStateResponse)
	require.True(t, ok)
	require.IsType(t, &Completed{}, stateMsg.State)

	// A fresh coordinator with the same session store restores the
	// completed session without actor mailbox replay.
	restarted := NewClientCoordinator(ClientActorCfg{
		OutboxHandler: &testOutboxHandler{
			t:              t,
			clientSigner:   clientSigner,
			operatorSigner: operatorSigner,
		},
		PackageStore: packageStore,
		SessionStore: sessionStore,
		ActorID:      "oor-client-coordinator-test",
	})
	require.NoError(t, restarted.Start(ctx))

	findResp := restarted.Receive(
		ctx, &FindOutgoingSessionByIdempotencyKeyRequest{
			IdempotencyKey: "send-once",
		},
	)
	require.True(t, findResp.IsOk())
	findMsg, ok :=
		findResp.UnwrapOr(nil).(*FindOutgoingSessionByIdempotencyKeyResponse)
	require.True(t, ok)
	require.True(t, findMsg.Found)
	require.Equal(t, startMsg.SessionID, findMsg.SessionID)
}
