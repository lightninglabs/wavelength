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

	inputs := []oortx.CheckpointInput{{
		Outpoint: wire.OutPoint{
			Hash:  [32]byte{0x01},
			Index: 0,
		},
		WitnessUtxo: &wire.TxOut{
			Value:    int64(inputValue),
			PkScript: []byte{0x51},
		},
		OwnerLeafScript: []byte{0x51},
	}}

	outputs := []oortx.RecipientOutput{{
		PkScript: []byte{0x51},
		Value:    inputValue,
	}}

	session, outbox, err := NewSession(ctx, policy, inputs, outputs)
	require.NoError(t, err)
	require.NotNil(t, session)
	require.NotEmpty(t, outbox)

	require.Len(t, outbox, 1)
	submit, ok := outbox[0].(*SendSubmitPackageRequest)
	require.True(t, ok)
	require.NotNil(t, submit.ArkPSBT)
	require.NotEmpty(t, submit.CheckpointPSBTs)

	state, err := session.FSM.CurrentState()
	require.NoError(t, err)
	_, ok = state.(*AwaitingSubmitAccepted)
	require.True(t, ok)

	// Step 1: Server accepts submit and returns co-signed checkpoints.
	fut := session.FSM.AskEvent(ctx, &SubmitAcceptedEvent{
		SessionID:               session.ID,
		ArkPSBT:                 submit.ArkPSBT,
		CoSignedCheckpointPSBTs: submit.CheckpointPSBTs,
	})
	result := fut.Await(ctx)
	require.False(t, result.IsErr())

	submitOutbox := result.UnwrapOr(nil)
	require.Len(t, submitOutbox, 1)
	_, ok = submitOutbox[0].(*RequestCheckpointSignatures)
	require.True(t, ok)

	// Step 2: Wallet attaches client signatures to checkpoints.
	finalCheckpoints := submit.CheckpointPSBTs
	finalCheckpoints[0].Inputs[0].TaprootKeySpendSig = []byte{0x01}

	fut = session.FSM.AskEvent(ctx, &CheckpointsSignedEvent{
		FinalCheckpointPSBTs: finalCheckpoints,
	})
	result = fut.Await(ctx)
	require.False(t, result.IsErr())

	finalizeOutbox := result.UnwrapOr(nil)
	require.Len(t, finalizeOutbox, 1)
	_, ok = finalizeOutbox[0].(*SendFinalizePackageRequest)
	require.True(t, ok)

	// Step 3: Server accepts finalize and updates VTXO set.
	fut = session.FSM.AskEvent(ctx, &FinalizeAcceptedEvent{})
	result = fut.Await(ctx)
	require.False(t, result.IsErr())

	markOutbox := result.UnwrapOr(nil)
	require.Len(t, markOutbox, 1)
	_, ok = markOutbox[0].(*MarkInputsSpentRequest)
	require.True(t, ok)

	// Step 4: Client persists that inputs are spent.
	fut = session.FSM.AskEvent(ctx, &InputsMarkedSpentEvent{})
	result = fut.Await(ctx)
	require.False(t, result.IsErr())
	require.Empty(t, result.UnwrapOr(nil))

	state, err = session.FSM.CurrentState()
	require.NoError(t, err)
	_, ok = state.(*Completed)
	require.True(t, ok)
}
