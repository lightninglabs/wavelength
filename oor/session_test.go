package oor

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/scripts"
	oortx "github.com/lightninglabs/darepo-client/lib/tx/oor"
	"github.com/lightningnetwork/lnd/input"
	"github.com/stretchr/testify/require"
)

// TestSessionHappyPath exercises the outgoing transfer FSM without the actor
// wrapper.
//
// It asserts the canonical v0 phase progression and final terminal state.
func TestSessionHappyPath(t *testing.T) {
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

	outputs := []oortx.RecipientOutput{
		{
			PkScript: newTestTaprootPkScript(t, clientKey.PubKey()),
			Value:    inputValue,
		},
	}

	session, outbox, err := NewSession(ctx, policy, inputs, outputs)
	require.NoError(t, err)
	require.NotNil(t, session)
	require.NotEmpty(t, outbox)

	// Creating a new session first emits an Ark-sign outbox request.
	// The FSM itself never calls the signer/network; the caller must perform
	// the side effect and then feed the result back as an event.
	require.Len(t, outbox, 1)
	arkSignReq, ok := outbox[0].(*RequestArkSignatures)
	require.True(t, ok)
	require.NotNil(t, arkSignReq.ArkPSBT)

	state, err := session.FSM.CurrentState()
	require.NoError(t, err)
	_, ok = state.(*AwaitingArkSignatures)
	require.True(t, ok)

	// Step 1: Ark signatures are attached before submit is sent.
	fut := session.FSM.AskEvent(ctx, &ArkSignedEvent{
		ArkPSBT: arkSignReq.ArkPSBT,
	})
	result := fut.Await(ctx)
	require.False(t, result.IsErr())

	submitOutbox := result.UnwrapOr(nil)
	require.Len(t, submitOutbox, 1)
	submit, ok := submitOutbox[0].(*SendSubmitPackageRequest)
	require.True(t, ok)
	require.NotNil(t, submit.ArkPSBT)
	require.NotEmpty(t, submit.CheckpointPSBTs)

	// Step 2: Server accepts submit and returns co-signed checkpoints.
	fut = session.FSM.AskEvent(ctx, &SubmitAcceptedEvent{
		SessionID:               session.ID,
		ArkPSBT:                 submit.ArkPSBT,
		CoSignedCheckpointPSBTs: submit.CheckpointPSBTs,
	})
	result = fut.Await(ctx)
	require.False(t, result.IsErr())

	signOutbox := result.UnwrapOr(nil)
	require.Len(t, signOutbox, 1)
	signReq, ok := signOutbox[0].(*RequestCheckpointSignatures)
	require.True(t, ok)
	require.NotEmpty(t, signReq.TransferInputs)

	// Step 3: Wallet attaches client signatures to checkpoints.
	//
	// This is the local signing boundary: no network calls are required,
	// but the signing implementation is still modeled as a side effect
	// outside the FSM for determinism and testability.
	err = SignCheckpointPSBTs(
		clientSigner, signReq.TransferInputs,
		signReq.CoSignedCheckpointPSBTs,
	)
	require.NoError(t, err)

	fut = session.FSM.AskEvent(ctx, &CheckpointsSignedEvent{
		FinalCheckpointPSBTs: signReq.CoSignedCheckpointPSBTs,
	})
	result = fut.Await(ctx)
	require.False(t, result.IsErr())

	finalizeOutbox := result.UnwrapOr(nil)
	require.Len(t, finalizeOutbox, 1)
	_, ok = finalizeOutbox[0].(*SendFinalizePackageRequest)
	require.True(t, ok)

	// Step 4: Server accepts finalize and updates VTXO set.
	fut = session.FSM.AskEvent(ctx, &FinalizeAcceptedEvent{})
	result = fut.Await(ctx)
	require.False(t, result.IsErr())

	markOutbox := result.UnwrapOr(nil)
	require.Len(t, markOutbox, 1)
	_, ok = markOutbox[0].(*MarkInputsSpentRequest)
	require.True(t, ok)

	// Step 5: Client persists that inputs are spent.
	fut = session.FSM.AskEvent(ctx, &InputsMarkedSpentEvent{})
	result = fut.Await(ctx)
	require.False(t, result.IsErr())
	require.Empty(t, result.UnwrapOr(nil))

	state, err = session.FSM.CurrentState()
	require.NoError(t, err)
	_, ok = state.(*Completed)
	require.True(t, ok)
}
