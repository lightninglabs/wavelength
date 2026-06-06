package unroll

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/darepo-client/unrollplan"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

type scriptedFeeInputPlanner struct {
	plans []FeeInputPlan
	calls int
}

// requireUnrollState unwraps protofsm's generic state into the unroll state
// interface for assertions.
func requireUnrollState(t *testing.T, raw any) State {
	t.Helper()

	state, ok := raw.(State)
	require.True(t, ok)

	return state
}

// PlanFeeInputs returns the next scripted plan.
func (p *scriptedFeeInputPlanner) PlanFeeInputs(_ context.Context,
	ready []unrollplan.TxFrontier) (FeeInputPlan, error) {

	p.calls++
	if len(p.plans) == 0 {
		return FeeInputPlan{
			RequiredFeeInputsNow: len(ready),
		}, nil
	}

	plan := p.plans[0]
	p.plans = p.plans[1:]
	plan.RequiredFeeInputsNow = len(ready)

	return plan, nil
}

func TestFeeInputFanoutPausesReadyFrontier(t *testing.T) {
	t.Parallel()

	proof := buildMergeProof(t)
	planner, err := unrollplan.NewPlanner(proof)
	require.NoError(t, err)

	feePlanner := &scriptedFeeInputPlanner{
		plans: []FeeInputPlan{{
			UsableFeeInputs:     1,
			FanoutOutputsNeeded: 2,
			RecommendedFanoutOutputAmountSat: btcutil.Amount(
				10_000,
			),
		}},
	}
	job := &JobState{Height: 100}

	transition, err := deriveStateTransition(
		t.Context(), job, &Environment{
			Proof:           proof,
			Planner:         planner,
			FeeInputPlanner: feePlanner,
		}, false, fn.None[int32](),
	)
	require.NoError(t, err)

	_, ok := transition.NextState.(*AwaitingFeeInputFanout)
	require.True(t, ok)
	require.Empty(
		t, stateJob(requireUnrollState(t, transition.NextState)).
			PlannerState.InFlightTxids,
	)

	outbox := transition.NewEvents.UnsafeFromSome().Outbox
	require.Len(t, outbox, 1)
	request, ok := outbox[0].(*RequestFeeInputFanout)
	require.True(t, ok)
	require.Len(t, request.Txids, 2)
	require.Equal(t, 2, request.Plan.FanoutOutputsNeeded)
	require.Equal(t, 1, request.Plan.UsableFeeInputs)
}

func TestFeeInputFanoutConfirmationReplans(t *testing.T) {
	t.Parallel()

	proof := buildMergeProof(t)
	planner, err := unrollplan.NewPlanner(proof)
	require.NoError(t, err)

	recommended := btcutil.Amount(10_000)
	feePlanner := &scriptedFeeInputPlanner{
		plans: []FeeInputPlan{
			{
				UsableFeeInputs:                  1,
				FanoutOutputsNeeded:              2,
				RecommendedFanoutOutputAmountSat: recommended,
			},
			{
				UsableFeeInputs: 2,
			},
		},
	}
	env := &Environment{
		Proof:           proof,
		Planner:         planner,
		FeeInputPlanner: feePlanner,
	}

	first, err := deriveStateTransition(
		t.Context(), &JobState{
			Height: 100,
		},
		env,
		false,
		fn.None[int32](),
	)
	require.NoError(t, err)

	state, ok := first.NextState.(*AwaitingFeeInputFanout)
	require.True(t, ok)

	next, err := state.ProcessEvent(
		t.Context(), &FeeInputsAvailableEvent{
			Height: 101,
		},
		env,
	)
	require.NoError(t, err)

	_, ok = next.NextState.(*AwaitingMaterialization)
	require.True(t, ok)
	require.Len(
		t, stateJob(requireUnrollState(t, next.NextState)).
			PlannerState.InFlightTxids,
		2,
	)

	outbox := next.NewEvents.UnsafeFromSome().Outbox
	require.Len(t, outbox, 1)
	_, ok = outbox[0].(*EnsureReadyTransactions)
	require.True(t, ok)
	require.Equal(t, 2, feePlanner.calls)
}

func TestFeeInputFanoutHeightUpdateReplansWhenInputsAvailable(t *testing.T) {
	t.Parallel()

	proof := buildMergeProof(t)
	planner, err := unrollplan.NewPlanner(proof)
	require.NoError(t, err)

	feePlanner := &scriptedFeeInputPlanner{
		plans: []FeeInputPlan{{
			UsableFeeInputs: 2,
		}},
	}
	state := &AwaitingFeeInputFanout{
		Job: &JobState{
			Height: 100,
		},
	}

	transition, err := state.ProcessEvent(
		t.Context(), &HeightUpdatedEvent{
			Height: 101,
		}, &Environment{
			Proof:           proof,
			Planner:         planner,
			FeeInputPlanner: feePlanner,
		},
	)
	require.NoError(t, err)

	_, ok := transition.NextState.(*AwaitingMaterialization)
	require.True(t, ok)
	require.Equal(t, 1, feePlanner.calls)
}

func TestFeeInputFanoutShortfallFails(t *testing.T) {
	t.Parallel()

	proof := buildMergeProof(t)
	planner, err := unrollplan.NewPlanner(proof)
	require.NoError(t, err)

	feePlanner := &scriptedFeeInputPlanner{
		plans: []FeeInputPlan{{
			UsableFeeInputs:           1,
			FanoutFundingShortfallSat: btcutil.Amount(5_000),
		}},
	}

	transition, err := deriveStateTransition(
		t.Context(), &JobState{Height: 100}, &Environment{
			Proof:           proof,
			Planner:         planner,
			FeeInputPlanner: feePlanner,
		}, false, fn.None[int32](),
	)
	require.NoError(t, err)

	_, ok := transition.NextState.(*Failed)
	require.True(t, ok)
	require.Contains(
		t, stateJob(requireUnrollState(t, transition.NextState)).
			FailReason, "insufficient wallet balance",
	)
}
