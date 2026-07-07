package oor

import (
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/wire/v2"
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
		ArkPSBT:              ark,
		FinalCheckpointPSBTs: checkpoints,
		TransferInputs: []TransferInput{
			input,
		},
	}

	snapshot, err := NewOutgoingSnapshot(state.SessionID, state)
	require.NoError(t, err)

	require.Equal(t, OutgoingPhaseFinalizeSent, snapshot.Phase)
	require.NotEmpty(t, snapshot.ArkPSBT)
	require.NotEmpty(t, snapshot.CheckpointPSBTs)
	require.NotNil(t, snapshot.TransferInputSnapshots)
	require.Len(t, snapshot.TransferInputSnapshots, 1)
}

// TestSnapshotRetryMetadataRoundTrip verifies that RetryAfter and retry
// reason survive TLV encode/decode. This is essential for restart-safe
// retry scheduling: the actor persists retry metadata alongside the real
// protocol state so a restarted actor can schedule a timer instead of
// immediately re-driving the outbox.
func TestSnapshotRetryMetadataRoundTrip(t *testing.T) {
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
			Hash:  [32]byte{0x02},
			Index: 0,
		}, inputValue,
	)

	recipients := []oortx.RecipientOutput{
		{
			PkScript: newTestTaprootPkScript(
				t, clientKey.PubKey(),
			),
			Value: inputValue,
		},
	}

	ark, checkpoints, err := buildSubmitPackage(
		policy, []TransferInput{input}, recipients,
	)
	require.NoError(t, err)

	sessionID := SessionID(ark.UnsignedTx.TxHash())

	state := &AwaitingSubmitAccepted{
		ArkPSBT:         ark,
		CheckpointPSBTs: checkpoints,
		TransferInputs: []TransferInput{
			input,
		},
		IdempotencyKey: "funding-key-1",
	}

	// Create a snapshot and apply retry metadata (simulating what the
	// actor does when a retryable outbox error occurs).
	snapshot, err := NewOutgoingSnapshot(sessionID, state)
	require.NoError(t, err)
	require.Equal(t, OutgoingPhaseSubmitSent, snapshot.Phase)

	snapshot.RetryAfter = 3 * time.Second
	snapshot.FailReason = "temporary transport error"

	// Encode and decode the snapshot to simulate checkpoint
	// persistence.
	raw, err := encodeOutgoingSnapshot(snapshot)
	require.NoError(t, err)

	decoded, err := decodeOutgoingSnapshot(raw)
	require.NoError(t, err)

	require.Equal(t, OutgoingPhaseSubmitSent, decoded.Phase)
	require.Equal(t, 3*time.Second, decoded.RetryAfter)
	require.Equal(t, "temporary transport error", decoded.FailReason)
	require.Equal(t, "funding-key-1", decoded.IdempotencyKey)

	// Verify the decoded snapshot can restore the original state.
	restored, err := OutgoingStateFromSnapshot(decoded)
	require.NoError(t, err)
	restoredSubmit, ok := restored.(*AwaitingSubmitAccepted)
	require.True(t, ok)
	require.Equal(t, "funding-key-1",
		restoredSubmit.IdempotencyKey)
}
