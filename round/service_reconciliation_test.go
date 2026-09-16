package round

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/lightninglabs/wavelength/rpc/roundpb"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// serviceReconcileFixture creates an admitted operation with bounded consent.
func serviceReconcileFixture() (*ServiceReconcileState, *ClientEnvironment) {
	return &ServiceReconcileState{
			RoundID: RoundID{
				2,
			},
			Intents: Intents{Service: &types.ServiceRequest{
				OperationID: [32]byte{
					1,
				}, Mode: types.ServiceScheduled,
				ScheduleVersion: 1, SlotIndex: 2,
				AllowFallback: true,
				ExpiresAtUnix: uint64(
					time.Now().Add(time.Hour).Unix(),
				),
			}},
		}, &ClientEnvironment{
			RoundKey: RoundKeyStr(
				RoundID{2}.KeyString(),
			),
			StatusReconcileTimeout: time.Second,
			ParticipationDeadline:  time.Now().Add(-time.Second),
		}
}

// TestServiceReconcileBudget preserves reservations and never retries execution
// when the operator is silent, even if signing messages arrive after expiry.
func TestServiceReconcileBudget(t *testing.T) {
	s, env := serviceReconcileFixture()
	for i := 0; i < serviceReconcileLimit; i++ {
		tr, err := s.ProcessEvent(
			t.Context(), &RegistrationTimedOut{}, env,
		)
		require.NoError(t, err)
		outbox := tr.NewEvents.UnwrapOr(ClientEmittedEvent{}).Outbox
		require.Len(t, outbox, 2)
		probe, ok := outbox[0].(*QueryRoundStatusOutbox)
		require.True(t, ok)
		require.Equal(
			t, s.Intents.Service.OperationID, probe.OperationID,
		)
		next, ok := tr.NextState.(*ServiceReconcileState)
		require.True(t, ok)
		s = next
	}
	tr, err := s.ProcessEvent(t.Context(), &RegistrationTimedOut{}, env)
	require.NoError(t, err)
	require.True(t, tr.NewEvents.IsNone())
	require.Equal(t, "ServiceDeferred", tr.NextState.String())
	tr, err = s.ProcessEvent(t.Context(), &GeneratePartialSigs{}, env)
	require.NoError(t, err)
	require.True(t, tr.NewEvents.IsNone())
	require.Equal(t, "ServiceDeferred", tr.NextState.String())
}

// TestServiceReconcileFallback requires an authoritative same-operation
// failure.
func TestServiceReconcileFallback(t *testing.T) {
	s, env := serviceReconcileFixture()
	report := &RoundStatusReported{
		RoundID: s.RoundID, Status: roundStatusDead,
		Operation: &roundpb.OperationStatus{
			OperationId: s.Intents.Service.OperationID[:],
			Mode:        roundpb.ServiceMode_SERVICE_SCHEDULED,
			Phase:       operationUncertain,
		},
	}
	tr, err := s.ProcessEvent(t.Context(), report, env)
	require.NoError(t, err)
	require.Same(t, s, tr.NextState)
	require.True(t, tr.NewEvents.IsNone())
	report.Operation.Phase = operationFailed
	tr, err = s.ProcessEvent(t.Context(), report, env)
	require.NoError(t, err)
	pending, ok := tr.NextState.(*PendingRoundAssembly)
	require.True(t, ok)
	require.Equal(t, types.ServiceImmediate, pending.Service.Mode)
	require.Equal(
		t, s.Intents.Service.OperationID, pending.Service.OperationID,
	)
	require.Equal(
		t, s.Intents.Service.ExpiresAtUnix,
		pending.Service.ExpiresAtUnix,
	)
	require.Equal(
		t, s.Intents.Service.FeeLimitSat, pending.Service.FeeLimitSat,
	)
	require.Empty(t, tr.NewEvents.UnwrapOr(ClientEmittedEvent{}).Outbox)
	require.True(t, env.ParticipationDeadline.IsZero())

	report.RoundID = RoundID{3}
	tr, err = s.ProcessEvent(t.Context(), report, env)
	require.NoError(t, err)
	require.Same(t, s, tr.NextState)
	report.RoundID = s.RoundID
	s.Intents.Service.AllowFallback = false
	tr, err = s.ProcessEvent(t.Context(), report, env)
	require.NoError(t, err)
	_, failed := tr.NextState.(*ClientFailedState)
	require.True(t, failed)
}

// TestOperationReportCannotReleaseUncertain rejects a contradictory dead
// answer.
func TestOperationReportCannotReleaseUncertain(t *testing.T) {
	roundID := RoundID{1}
	report := &roundpb.ClientRoundStatusReport{
		RoundId: roundID[:], Status: roundStatusDead,
		Operation: &roundpb.OperationStatus{
			OperationId: make([]byte, 32), RoundId: roundID[:],
			Attempt: 1, Mode: roundpb.ServiceMode_SERVICE_IMMEDIATE,
			ParticipationDeadlineUnix: 100,
			Phase:                     operationUncertain,
		},
	}
	report.Operation.OperationId[0] = 1
	require.ErrorContains(t, validateOperationReport(report), "dead round")
	report.Operation.Phase = operationFailed
	require.NoError(t, validateOperationReport(report))
	decoded := &RoundStatusReported{}
	require.NoError(t, decoded.FromProto(report))
	report.Operation.OperationId[0] = 2
	require.Equal(t, byte(1), decoded.Operation.OperationId[0])
}

// TestExpiredAdmissionReconciles preserves the assigned operation even when
// the accepted acknowledgement arrives after its participation deadline.
func TestExpiredAdmissionReconciles(t *testing.T) {
	s, env := serviceReconcileFixture()
	env.ParticipationDeadline = time.Time{}
	state := &IntentSentState{Intents: s.Intents}
	now := time.Now().Unix()
	accepted := roundpb.AdmissionCode_ADMISSION_ACCEPTED
	opID := s.Intents.Service.OperationID
	joined := &RoundJoined{RoundID: s.RoundID,
		Admission: &roundpb.ServiceAdmission{
			OperationId:               opID[:],
			Attempt:                   1,
			Code:                      accepted,
			Mode:                      scheduledServiceMode,
			ParticipationDeadlineUnix: now - 1,
			AssignedSlot: &roundpb.SlotOpportunity{
				ScheduleVersion: 1, SlotIndex: 2,
				RegistrationOpenUnix:      now - 120,
				RegistrationCloseUnix:     now - 60,
				ParticipationDeadlineUnix: now - 1,
			},
		},
	}
	tr, err := state.ProcessEvent(t.Context(), joined, env)
	require.NoError(t, err)
	reconciling, ok := tr.NextState.(*ServiceReconcileState)
	require.True(t, ok)
	require.Equal(t, s.RoundID, reconciling.RoundID)
	events := tr.NewEvents.UnwrapOr(ClientEmittedEvent{})
	for _, message := range events.Outbox {
		_, release := message.(*ReleaseForfeitReservation)
		require.False(t, release)
	}
}

// coldServiceStore supplies durable records without any signing data.
type coldServiceStore struct {
	ServiceOperationStore
	operations []DeferredServiceOperation
	completed  [][32]byte
}

// ListDeferredServiceOperations returns the stored ownership records.
func (s *coldServiceStore) ListDeferredServiceOperations(context.Context) (
	[]DeferredServiceOperation, error) {

	return s.operations, nil
}

// CompleteServiceOperation records only authoritative safe retirement.
func (s *coldServiceStore) CompleteServiceOperation(_ context.Context,
	id [32]byte) error {

	s.completed = append(s.completed, id)

	return nil
}

// TestColdServiceRestoreNeverSigns restores a bounded status probe
// and refuses automatic fallback even when a later reply reports safe failure.
func TestColdServiceRestoreNeverSigns(t *testing.T) {
	h := newActorTestHarness(t)
	state, _ := serviceReconcileFixture()
	h.actor.cfg.ServiceStore = &coldServiceStore{
		operations: []DeferredServiceOperation{{
			Service: *state.Intents.Service, RoundID: state.RoundID,
		}},
	}
	h.actor.env.ServiceStore = h.actor.cfg.ServiceStore
	require.NoError(t, h.actor.restoreDeferredServiceOperations(h.ctx))
	target := h.actor.rounds[RoundKeyStr(state.RoundID.KeyString())]
	require.NotNil(t, target)
	t.Cleanup(target.FSM.Stop)
	recovered, err := fsmState(h.ctx, target.FSM)
	require.NoError(t, err)
	cold, ok := recovered.(*ServiceReconcileState)
	require.True(t, ok)
	require.True(t, cold.Cold)
	require.Equal(t, "ServiceReconcile", cold.String())
	report := &RoundStatusReported{
		RoundID: state.RoundID,
		Operation: &roundpb.OperationStatus{
			OperationId: state.Intents.Service.OperationID[:],
			Mode:        scheduledServiceMode,
			Phase:       operationFailed,
		},
	}
	tr, err := cold.ProcessEvent(h.ctx, report, h.actor.env)
	require.NoError(t, err)
	_, failed := tr.NextState.(*ClientFailedState)
	require.True(t, failed)
	_, joined := findOutbox[*JoinRoundRequest](
		tr.NewEvents.UnwrapOr(ClientEmittedEvent{}).Outbox,
	)
	require.False(t, joined)
	store, ok := h.actor.cfg.ServiceStore.(*coldServiceStore)
	require.True(t, ok)
	require.Equal(
		t, [][32]byte{state.Intents.Service.OperationID},
		store.completed,
	)
}

// TestServiceDeadlinePreservesCheckpoint verifies that slow checkpoint work
// crossing the deadline retains recovery effects without exporting signatures.
func TestServiceDeadlinePreservesCheckpoint(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		state, env := serviceReconcileFixture()
		env.ParticipationDeadline = time.Now().Add(time.Second)
		checkpoint := &InputSigSentState{Intents: state.Intents}
		confirmation := &RegisterConfirmationRequest{}
		timer := &StartTimeoutReq{}
		transition := &ClientStateTransition{
			NextState: checkpoint,
			NewEvents: fn.Some(ClientEmittedEvent{
				Outbox: []ClientOutMsg{
					&SubmitForfeitSigRequest{},
					confirmation,
					&SubmitVTXOForfeitSigsToServer{}, timer,
				},
			}),
		}

		// Model work returning only after the fixed assignment expired.
		time.Sleep(2 * time.Second)
		got, err := fenceServiceTransition(
			t.Context(), state, transition, nil, state.Intents,
			state.RoundID, env,
		)
		require.NoError(t, err)
		require.Same(t, checkpoint, got.NextState)
		require.Equal(
			t, []ClientOutMsg{confirmation, timer},
			got.NewEvents.UnwrapOr(ClientEmittedEvent{}).Outbox,
		)
	})
}

// TestServiceDeadlineRetainsUnsignedOwnership checks that work returning after
// cancellation cannot publish protocol progress or release reserved inputs.
func TestServiceDeadlineRetainsUnsignedOwnership(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		state, env := serviceReconcileFixture()
		env.ParticipationDeadline = time.Now().Add(time.Second)
		ctx, cancel := env.participationContext(t.Context())
		defer cancel()
		time.Sleep(2 * time.Second)
		require.ErrorIs(t, ctx.Err(), context.DeadlineExceeded)
		transition := &ClientStateTransition{
			NextState: &PartialSigsSentState{},
			NewEvents: fn.Some(
				ClientEmittedEvent{
					Outbox: []ClientOutMsg{
						&SubmitPartialSigRequest{},
					},
				},
			),
		}
		got, err := fenceServiceTransition(
			t.Context(), state, transition, nil, state.Intents,
			state.RoundID, env,
		)
		require.NoError(t, err)
		reconcile, ok := got.NextState.(*ServiceReconcileState)
		require.True(t, ok)
		require.Equal(
			t, state.Intents.Service, reconcile.Intents.Service,
		)
		_, signed := findOutbox[*SubmitPartialSigRequest](
			got.NewEvents.UnwrapOr(ClientEmittedEvent{}).Outbox,
		)
		require.False(t, signed)
	})
}
