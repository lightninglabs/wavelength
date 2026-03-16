package oor

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	oortx "github.com/lightninglabs/darepo-client/lib/tx/oor"
	"github.com/stretchr/testify/require"
)

// TestNewOutgoingSnapshotFinalizeSentMinimality verifies finalize-sent
// snapshots persist only the artifacts needed for deterministic retry/resume.
func TestNewOutgoingSnapshotFinalizeSentMinimality(t *testing.T) {
	t.Parallel()

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	policy := arkscript.CheckpointPolicy{
		OperatorKey: operatorKey.PubKey(),
		CSVDelay:    10,
	}

	const inputValue = btcutil.Amount(10000)

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	input := newTestTransferInput(
		t, clientKey, policy.OperatorKey, wire.OutPoint{
			Hash:  [32]byte{0x01},
			Index: 0,
		}, inputValue,
	)

	recipients := []oortx.RecipientOutput{
		{
			PkScript: newTestTaprootPkScript(t, clientKey.PubKey()),
			Value:    inputValue,
		},
	}

	ark, checkpoints, err := buildSubmitPackage(
		policy, []TransferInput{input}, recipients,
	)
	require.NoError(t, err)

	state := &AwaitingFinalizeAccepted{
		SessionID:            SessionID(ark.UnsignedTx.TxHash()),
		InputOutpoints:       []wire.OutPoint{input.VTXO.Outpoint},
		ArkPSBT:              ark,
		FinalCheckpointPSBTs: checkpoints,
	}

	snapshot, err := NewOutgoingSnapshot(state.SessionID, state)
	require.NoError(t, err)

	require.Equal(t, OutgoingPhaseFinalizeSent, snapshot.Phase)
	require.NotEmpty(t, snapshot.ArkPSBT)
	require.NotEmpty(t, snapshot.CheckpointPSBTs)
	require.Equal(t, state.InputOutpoints, snapshot.InputOutpoints)

	// Finalize retries do not require transfer input material, so the
	// snapshot should not carry those fields in this phase.
	require.Nil(t, snapshot.TransferInputs)
	require.Nil(t, snapshot.TransferInputSnapshots)
}
