package unroll

import (
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/darepo-client/unrollplan"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// TestCheckpointRoundTripByPhase verifies that durable checkpoints restore to
// the expected concrete protofsm state shape.
func TestCheckpointRoundTripByPhase(t *testing.T) {
	targetTxid := chainhash.Hash{0xAA}
	sweepTxid := chainhash.Hash{0xBB}

	deferred := []DeferredCheckpoint{{
		Txid: chainhash.Hash{
			0x02,
		},
		DeadlineHeight: 220,
	}}

	testCases := []struct {
		name  string
		state State
		typ   interface{}
	}{
		{
			name: "materialization",
			state: &AwaitingMaterialization{
				Job: &JobState{
					Height:         100,
					Trigger:        TriggerFraudSpend,
					ExitPolicyKind: "vhtlc_claim",
					ExitPolicyRef:  "recovery-policy-ref",
					PlannerState: unrollState(
						chainhash.Hash{0x01},
						fn.None[int32](), nil,
					),
					DeferredCheckpoints: deferred,
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
						targetTxid, fn.Some[int32](103),
						nil,
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
						targetTxid, fn.Some[int32](103),
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
						targetTxid, fn.Some[int32](103),
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
						targetTxid, fn.None[int32](),
						nil,
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
				t, stateHeight(tc.state), stateHeight(restored),
			)
			require.Equal(
				t, stateTrigger(tc.state),
				stateTrigger(restored),
			)
			require.Equal(
				t, exitPolicyKind(
					stateJob(tc.state).ExitPolicyKind,
				),
				exitPolicyKind(
					stateJob(restored).ExitPolicyKind,
				),
			)
			require.Equal(
				t, stateJob(tc.state).ExitPolicyRef,
				stateJob(restored).ExitPolicyRef,
			)
			require.Equal(
				t, stateJob(tc.state).FailReason,
				stateJob(restored).FailReason,
			)
			require.Equal(
				t, stateJob(tc.state).PlannerState,
				stateJob(restored).PlannerState,
			)
			require.Equal(
				t, stateJob(tc.state).DeferredCheckpoints,
				stateJob(restored).DeferredCheckpoints,
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
		ConfirmHeight: fn.Some[int32](110),
	}
	state := &Completed{
		Job: &JobState{
			Height:  106,
			Trigger: TriggerManual,
			PlannerState: unrollplan.State{
				ConfirmedTxids: []chainhash.Hash{
					targetTxid,
				},
				TargetConfirmHeight: fn.Some[int32](103),
				Sweep:               sweepState,
			},
		},
	}

	checkpoint := checkpointFromState(state, sweepTx)

	require.True(t, checkpoint.State.Sweep.Txid.IsSome())
	require.Equal(
		t, sweepTxid, checkpoint.State.Sweep.Txid.UnsafeFromSome(),
	)
}

// unrollState builds a minimal planner state for checkpoint tests.
func unrollState(targetTxid chainhash.Hash, targetHeight fn.Option[int32],
	sweepTxid *chainhash.Hash) unrollplan.State {

	state := unrollplan.State{
		ConfirmedTxids: []chainhash.Hash{
			targetTxid,
		},
		TargetConfirmHeight: targetHeight,
	}

	if sweepTxid != nil {
		state.Sweep.Status = unrollplan.SweepStatusBroadcasted
		state.Sweep.Txid = fn.Some(*sweepTxid)
	}

	return state
}

// completedUnrollState builds a minimal completed planner state.
func completedUnrollState(targetTxid chainhash.Hash,
	targetHeight fn.Option[int32],
	sweepTxid chainhash.Hash) unrollplan.State {

	state := unrollState(targetTxid, targetHeight, &sweepTxid)
	state.Sweep.Status = unrollplan.SweepStatusConfirmed
	state.Sweep.ConfirmHeight = fn.Some[int32](110)

	return state
}
