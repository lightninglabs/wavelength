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

// TestNewOutgoingSnapshotFinalizeSentMinimality verifies finalize-sent
// snapshots persist only the artifacts needed for deterministic retry/resume.
func TestNewOutgoingSnapshotFinalizeSentMinimality(t *testing.T) {
	t.Parallel()

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	policy := scripts.CheckpointPolicy{
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
		ArkPSBT:              ark,
		FinalCheckpointPSBTs: checkpoints,
		TransferInputs:       []TransferInput{input},
	}

	snapshot, err := NewOutgoingSnapshot(state.SessionID, state)
	require.NoError(t, err)

	require.Equal(t, OutgoingPhaseFinalizeSent, snapshot.Phase)
	require.NotEmpty(t, snapshot.ArkPSBT)
	require.NotEmpty(t, snapshot.CheckpointPSBTs)
	require.NotNil(t, snapshot.TransferInputSnapshots)
	require.Len(t, snapshot.TransferInputSnapshots, 1)
}
