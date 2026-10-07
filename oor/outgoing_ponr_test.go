package oor

import (
	"testing"

	clientdb "github.com/lightninglabs/wavelength/db"
	"github.com/stretchr/testify/require"
)

// testPONRStates returns one outgoing state per FSM phase paired with whether a
// terminal failure out of it happens before the point of no return.
func testPONRStates(t *testing.T) []struct {
	name    string
	state   State
	prePONR bool
} {
	t.Helper()

	ark, checkpoints := testOutboxPSBTPair(t)
	inputs := testRetryTransferInputs(t)

	return []struct {
		name    string
		state   State
		prePONR bool
	}{
		{
			name:    "idle",
			state:   &Idle{},
			prePONR: true,
		},
		{
			name: "awaiting ark signatures",
			state: &AwaitingArkSignatures{
				ArkPSBT:         ark,
				CheckpointPSBTs: checkpoints,
				TransferInputs:  inputs,
				IdempotencyKey:  "key",
			},
			prePONR: true,
		},
		{
			name: "awaiting submit accepted",
			state: &AwaitingSubmitAccepted{
				ArkPSBT:         ark,
				CheckpointPSBTs: checkpoints,
				TransferInputs:  inputs,
				IdempotencyKey:  "key",
			},
			prePONR: true,
		},
		{
			name: "awaiting checkpoint signatures",
			state: &AwaitingCheckpointSignatures{
				ArkPSBT:                 ark,
				CoSignedCheckpointPSBTs: checkpoints,
				TransferInputs:          inputs,
				IdempotencyKey:          "key",
			},
		},
		{
			name: "awaiting finalize accepted",
			state: &AwaitingFinalizeAccepted{
				ArkPSBT:              ark,
				FinalCheckpointPSBTs: checkpoints,
				TransferInputs:       inputs,
				IdempotencyKey:       "key",
			},
		},
		{
			name: "awaiting local vtxo update",
			state: &AwaitingLocalVTXOUpdate{
				TransferInputs: inputs,
				IdempotencyKey: "key",
			},
		},
	}
}

// TestFailedRecordsPointOfNoReturn verifies every way the outgoing FSM reaches
// Failed records whether the failure was before the point of no return, and
// that only the states where the operator holds no co-signed spend say so.
func TestFailedRecordsPointOfNoReturn(t *testing.T) {
	t.Parallel()

	for _, test := range testPONRStates(t) {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			// A deterministic local failure drives FailEvent.
			failed, err := test.state.ProcessEvent(
				t.Context(), &FailEvent{
					Reason: "boom",
				},
				nil,
			)
			require.NoError(t, err)
			terminal, ok := failed.NextState.(*Failed)
			require.True(t, ok)
			require.Equal(t, test.prePONR, terminal.PrePONR)

			// A non-retryable outbox error takes the other route to
			// Failed, which also releases the pre-PONR inputs.
			if test.name == "idle" {
				return
			}
			failed, err = test.state.ProcessEvent(
				t.Context(), &OutboxErrorEvent{
					OutboxType:  "x",
					ErrorReason: "boom",
				}, &Environment{},
			)
			require.NoError(t, err)
			terminal, ok = failed.NextState.(*Failed)
			require.True(t, ok)
			require.Equal(t, test.prePONR, terminal.PrePONR)
		})
	}
}

// TestOutgoingFailedBeforePONRFromRecord verifies the persisted failed record
// reports the pre-PONR origin, and that every record that does not positively
// carry it, including one written before the origin was recorded, reads false.
func TestOutgoingFailedBeforePONRFromRecord(t *testing.T) {
	t.Parallel()

	ark, _ := testOutboxPSBTPair(t)
	sessionID, err := sessionIDFromArk(ark)
	require.NoError(t, err)

	record := func(state State) clientdb.OORSessionRegistryRecord {
		rec, err := outgoingRegistryRecord(sessionID, state)
		require.NoError(t, err)

		return rec
	}

	pre := record(&Failed{Reason: "rejected", PrePONR: true})
	got, err := OutgoingFailedBeforePONR(&pre)
	require.NoError(t, err)
	require.True(t, got)

	post := record(&Failed{Reason: "finalize lost"})
	got, err = OutgoingFailedBeforePONR(&post)
	require.NoError(t, err)
	require.False(t, got)

	// A snapshot from before the record existed ends without the trailing
	// record: type, length, and one value byte.
	legacy := record(&Failed{Reason: "old", PrePONR: true})
	legacy.SnapshotData = legacy.SnapshotData[:len(legacy.SnapshotData)-3]
	decoded, err := decodeOutgoingSnapshot(legacy.SnapshotData)
	require.NoError(t, err)
	require.False(t, decoded.FailedPrePONR)
	got, err = OutgoingFailedBeforePONR(&legacy)
	require.NoError(t, err)
	require.False(t, got)

	// A session that has not failed never reads as a pre-PONR failure.
	pending := record(&Completed{IdempotencyKey: "key"})
	got, err = OutgoingFailedBeforePONR(&pending)
	require.NoError(t, err)
	require.False(t, got)

	// The flag survives a restore from the snapshot.
	snapshot, err := decodeOutgoingSnapshot(pre.SnapshotData)
	require.NoError(t, err)
	state, err := OutgoingStateFromSnapshot(snapshot)
	require.NoError(t, err)
	restored, ok := state.(*Failed)
	require.True(t, ok)
	require.True(t, restored.PrePONR)
}
