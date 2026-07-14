package oor

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	oortx "github.com/lightninglabs/wavelength/lib/tx/oor"
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

	policy := arkscript.CheckpointPolicy{
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
	// The FSM itself never calls the signer/network.
	// The caller must perform the side effect and then feed the result
	// back as an event.
	require.Len(t, outbox, 1)
	arkSignReq, ok := outbox[0].(*RequestArkSignatures)
	require.True(t, ok)
	require.NotNil(t, arkSignReq.ArkPSBT)

	state, err := session.FSM.CurrentState()
	require.NoError(t, err)
	_, ok = state.(*AwaitingArkSignatures)
	require.True(t, ok)

	// Step 1: Ark signatures are attached before submit is sent.
	err = SignArkPSBT(
		clientSigner, arkSignReq.ArkPSBT, arkSignReq.CheckpointPSBTs,
		arkSignReq.TransferInputs,
	)
	require.NoError(t, err)

	fut := session.FSM.AskEvent(ctx, &ArkSignedEvent{
		ArkPSBT: arkSignReq.ArkPSBT,
	})
	result := fut.Await(ctx)
	require.False(t, result.IsErr())

	// The submit transition emits the submit request plus a re-drive timer
	// so a dead-lettered submit re-drives instead of wedging the session.
	submitOutbox := result.UnwrapOr(nil)
	require.Len(t, submitOutbox, 2)
	submit, ok := submitOutbox[0].(*SendSubmitPackageRequest)
	require.True(t, ok)
	require.NotNil(t, submit.ArkPSBT)
	require.NotEmpty(t, submit.CheckpointPSBTs)
	require.IsType(t, &ScheduleRetryRequest{}, submitOutbox[1])

	err = coSignCheckpointPSBTsForTest(
		operatorSigner, submit.TransferInputs, submit.CheckpointPSBTs,
	)
	require.NoError(t, err)

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

	// The finalize transition emits the finalize request plus a re-drive
	// timer so a dead-lettered finalize re-drives instead of wedging.
	finalizeOutbox := result.UnwrapOr(nil)
	require.Len(t, finalizeOutbox, 2)
	_, ok = finalizeOutbox[0].(*SendFinalizePackageRequest)
	require.True(t, ok)
	require.IsType(t, &ScheduleRetryRequest{}, finalizeOutbox[1])

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

// TestSessionMultiInputHappyPath verifies the outgoing transfer FSM with
// multiple VTXO inputs. This exercises the multi-input Ark signing path
// where BIP-341 sighash commits to ALL prevouts, requiring a
// MultiPrevOutFetcher rather than a CannedPrevOutputFetcher.
func TestSessionMultiInputHappyPath(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	policy := arkscript.CheckpointPolicy{
		OperatorKey: operatorKey.PubKey(),
		CSVDelay:    10,
	}

	// Use two distinct client keys and different amounts to ensure
	// the sighash computation correctly handles heterogeneous inputs.
	clientKey1, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	clientKey2, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	clientSigner := input.NewMockSigner(
		[]*btcec.PrivateKey{clientKey1, clientKey2}, nil,
	)
	operatorSigner := input.NewMockSigner(
		[]*btcec.PrivateKey{operatorKey}, nil,
	)

	input1Value := btcutil.Amount(10000)
	input2Value := btcutil.Amount(25000)

	inputs := []TransferInput{
		newTestTransferInput(
			t, clientKey1, policy.OperatorKey,
			wire.OutPoint{
				Hash:  [32]byte{0x01},
				Index: 0,
			},
			input1Value,
		),
		newTestTransferInput(
			t, clientKey2, policy.OperatorKey,
			wire.OutPoint{
				Hash:  [32]byte{0x02},
				Index: 0,
			},
			input2Value,
		),
	}

	outputs := []oortx.RecipientOutput{
		{
			PkScript: newTestTaprootPkScript(
				t, clientKey1.PubKey(),
			),
			Value: input1Value + input2Value,
		},
	}

	session, outbox, err := NewSession(ctx, policy, inputs, outputs)
	require.NoError(t, err)
	require.NotNil(t, session)
	require.Len(t, outbox, 1)

	arkSignReq, ok := outbox[0].(*RequestArkSignatures)
	require.True(t, ok)

	// The Ark tx should have 2 inputs (one per checkpoint).
	require.Len(
		t, arkSignReq.ArkPSBT.UnsignedTx.TxIn, 2,
		"Ark tx should have one input per VTXO",
	)
	require.Len(
		t, arkSignReq.CheckpointPSBTs, 2,
		"should have one checkpoint per VTXO",
	)

	// Sign the Ark PSBT with the client keys. This exercises the
	// MultiPrevOutFetcher path for correct BIP-341 sighash
	// computation across heterogeneous inputs.
	err = SignArkPSBT(
		clientSigner, arkSignReq.ArkPSBT, arkSignReq.CheckpointPSBTs,
		arkSignReq.TransferInputs,
	)
	require.NoError(t, err)

	// Verify the signed Ark PSBT has signatures on both inputs.
	for i, pInput := range arkSignReq.ArkPSBT.Inputs {
		require.NotEmpty(
			t, pInput.TaprootScriptSpendSig, "Ark input %d "+
				"should have a script spend sig", i,
		)
	}

	// Feed the signed Ark PSBT back into the FSM.
	fut := session.FSM.AskEvent(ctx, &ArkSignedEvent{
		ArkPSBT: arkSignReq.ArkPSBT,
	})
	result := fut.Await(ctx)
	require.False(
		t, result.IsErr(),
		"ArkSignedEvent should succeed: %v", result.Err(),
	)

	// The submit transition emits the submit request plus a re-drive timer.
	submitOutbox := result.UnwrapOr(nil)
	require.Len(t, submitOutbox, 2)
	submit, ok := submitOutbox[0].(*SendSubmitPackageRequest)
	require.True(t, ok)
	require.Len(t, submit.CheckpointPSBTs, 2)
	require.IsType(t, &ScheduleRetryRequest{}, submitOutbox[1])

	// Operator co-signs the checkpoints.
	err = coSignCheckpointPSBTsForTest(
		operatorSigner, submit.TransferInputs, submit.CheckpointPSBTs,
	)
	require.NoError(t, err)

	// Server accepts submit.
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

	// Client signs checkpoints.
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

	// Finalize.
	fut = session.FSM.AskEvent(ctx, &FinalizeAcceptedEvent{})
	result = fut.Await(ctx)
	require.False(t, result.IsErr())

	markOutbox := result.UnwrapOr(nil)
	require.Len(t, markOutbox, 1)
	_, ok = markOutbox[0].(*MarkInputsSpentRequest)
	require.True(t, ok)

	// Mark spent.
	fut = session.FSM.AskEvent(ctx, &InputsMarkedSpentEvent{})
	result = fut.Await(ctx)
	require.False(t, result.IsErr())

	state, err := session.FSM.CurrentState()
	require.NoError(t, err)
	_, ok = state.(*Completed)
	require.True(t, ok, "FSM should reach Completed state")
}
