package unroll

import (
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/unrollplan"
	"github.com/stretchr/testify/require"
)

// TestCheckpointRoundTripByPhase verifies that durable checkpoints restore to
// the expected concrete protofsm state shape.
func TestCheckpointRoundTripByPhase(t *testing.T) {
	targetTxid := chainhash.Hash{0xAA}
	sweepTxid := chainhash.Hash{0xBB}

	testCases := []struct {
		name  string
		state State
		typ   interface{}
	}{
		{
			name: "materialization",
			state: &AwaitingMaterialization{
				Job: &JobState{
					Height:  100,
					Trigger: TriggerManual,
					PlannerState: unrollState(
						chainhash.Hash{0x01}, nil, nil,
					),
				},
			},
			typ: &AwaitingMaterialization{},
		},
		{
			name: "csv_pending",
			state: &AwaitingCSV{
				Job: &JobState{
					Height:  104,
					Trigger: TriggerRestart,
					PlannerState: unrollState(
						targetTxid,
						copyHeight(103), nil,
					),
				},
			},
			typ: &AwaitingCSV{},
		},
		{
			name: "sweep_confirmation",
			state: &AwaitingSweepConfirmation{
				Job: &JobState{
					Height:  105,
					Trigger: TriggerManual,
					PlannerState: unrollState(
						targetTxid,
						copyHeight(103),
						&sweepTxid,
					),
				},
			},
			typ: &AwaitingSweepConfirmation{},
		},
		{
			name: "completed",
			state: &Completed{
				Job: &JobState{
					Height:  106,
					Trigger: TriggerManual,
					PlannerState: completedUnrollState(
						targetTxid,
						copyHeight(103),
						sweepTxid,
					),
				},
			},
			typ: &Completed{},
		},
		{
			name: "failed",
			state: &Failed{
				Job: &JobState{
					Height:  107,
					Trigger: TriggerRestart,
					PlannerState: unrollState(
						targetTxid, nil, nil,
					),
					FailReason: "boom",
				},
			},
			typ: &Failed{},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			checkpoint := checkpointFromState(tc.state, nil)
			restored := stateFromCheckpoint(checkpoint)

			require.IsType(t, tc.typ, restored)
			require.Equal(
				t, phaseFromState(tc.state),
				phaseFromState(restored),
			)
			require.Equal(
				t, stateHeight(tc.state),
				stateHeight(restored),
			)
			require.Equal(
				t, stateTrigger(tc.state),
				stateTrigger(restored),
			)
			require.Equal(
				t, stateJob(tc.state).FailReason,
				stateJob(restored).FailReason,
			)
			require.Equal(
				t, stateJob(tc.state).PlannerState,
				stateJob(restored).PlannerState,
			)
		})
	}
}

// TestCheckpointFromStateUsesStoredSweepTxid verifies that checkpointing keeps
// a terminal sweep txid observable even if the planner state is missing the
// hash but the actor still has the sweep transaction.
func TestCheckpointFromStateUsesStoredSweepTxid(t *testing.T) {
	targetTxid := chainhash.Hash{0xAA}
	sweepTx := wire.NewMsgTx(2)
	sweepTxid := sweepTx.TxHash()

	sweepState := unrollplan.SweepState{
		Status:        unrollplan.SweepStatusConfirmed,
		ConfirmHeight: copyHeight(110),
	}
	state := &Completed{
		Job: &JobState{
			Height:  106,
			Trigger: TriggerManual,
			PlannerState: unrollplan.State{
				ConfirmedTxids: []chainhash.Hash{
					targetTxid,
				},
				TargetConfirmHeight: copyHeight(103),
				Sweep:               sweepState,
			},
		},
	}

	checkpoint := checkpointFromState(state, sweepTx)

	require.NotNil(t, checkpoint.State.Sweep.Txid)
	require.Equal(t, sweepTxid, *checkpoint.State.Sweep.Txid)
}

// unrollState builds a minimal planner state for checkpoint tests.
func unrollState(targetTxid chainhash.Hash, targetHeight *int32,
	sweepTxid *chainhash.Hash) unrollplan.State {

	state := unrollplan.State{
		ConfirmedTxids: []chainhash.Hash{targetTxid},
	}

	if targetHeight != nil {
		state.TargetConfirmHeight = copyHeight(*targetHeight)
	}

	if sweepTxid != nil {
		hashCopy := *sweepTxid
		state.Sweep.Status = unrollplan.SweepStatusBroadcasted
		state.Sweep.Txid = &hashCopy
	}

	return state
}

// completedUnrollState builds a minimal completed planner state.
func completedUnrollState(targetTxid chainhash.Hash, targetHeight *int32,
	sweepTxid chainhash.Hash) unrollplan.State {

	state := unrollState(targetTxid, targetHeight, &sweepTxid)
	state.Sweep.Status = unrollplan.SweepStatusConfirmed
	state.Sweep.ConfirmHeight = copyHeight(110)

	return state
}
