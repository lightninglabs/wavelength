package round

import (
	"testing"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/baselib/protofsm"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/lightninglabs/wavelength/rpc/roundpb"
	"github.com/stretchr/testify/require"
)

// TestServiceAssignmentBinding rejects responses that change authorization.
func TestServiceAssignmentBinding(t *testing.T) {
	request := &types.ServiceRequest{
		OperationID: [32]byte{
			1,
		}, Mode: types.ServiceScheduled,
		ScheduleVersion: 3, SlotIndex: 9, ExpiresAtUnix: 300,
	}
	base := &roundpb.ServiceAdmission{
		Code:        roundpb.AdmissionCode_ADMISSION_ACCEPTED,
		OperationId: request.OperationID[:], Attempt: 1,
		Mode:                      scheduledServiceMode,
		ParticipationDeadlineUnix: 200,
		AssignedSlot: &roundpb.SlotOpportunity{
			ScheduleVersion: 3, SlotIndex: 9,
			RegistrationOpenUnix: 50, RegistrationCloseUnix: 100,
			ParticipationDeadlineUnix: 200,
		},
	}
	require.NoError(t, validateServiceAssignment(request, base))
	for _, test := range []struct {
		name   string
		mutate func(*roundpb.ServiceAdmission)
	}{
		{
			"operation",
			func(a *roundpb.ServiceAdmission) {
				a.OperationId[0]++
			},
		},
		{
			"attempt",
			func(a *roundpb.ServiceAdmission) {
				a.Attempt = 0
			},
		},
		{
			"mode",
			func(a *roundpb.ServiceAdmission) {
				a.Mode = roundpb.ServiceMode_SERVICE_IMMEDIATE
			},
		},
		{
			"slot",
			func(a *roundpb.ServiceAdmission) {
				a.AssignedSlot.SlotIndex++
			},
		},
		{
			"version",
			func(a *roundpb.ServiceAdmission) {
				a.AssignedSlot.ScheduleVersion++
			},
		},
		{
			"expiry",
			func(a *roundpb.ServiceAdmission) {
				a.ParticipationDeadlineUnix = 301
			},
		},
		{
			"slot deadline",
			func(a *roundpb.ServiceAdmission) {
				a.ParticipationDeadlineUnix = 201
			},
		},
		{
			"no signing window",
			func(a *roundpb.ServiceAdmission) {
				a.ParticipationDeadlineUnix = 100
			},
		},
		{
			"missing slot",
			func(a *roundpb.ServiceAdmission) {
				a.AssignedSlot = nil
			},
		},
		{
			"rejection",
			func(a *roundpb.ServiceAdmission) {
				a.Code = roundpb.AdmissionCode_ADMISSION_FULL
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			reply, err := decodeServiceAdmission(base, true)
			require.NoError(t, err)
			test.mutate(reply)
			require.Error(
				t, validateServiceAssignment(request, reply),
			)
		})
	}
	require.NoError(t, validateServiceAssignment(request, base))
	require.Error(t, validateServiceAssignment(request, nil))
	require.Error(t, validateServiceAssignment(nil, base))
	require.NoError(t, validateServiceAssignment(nil, nil))
}

// TestServiceRejectionFromProto preserves typed routing and retry information.
func TestServiceRejectionFromProto(t *testing.T) {
	pb := &roundpb.ClientErrorResp{
		ErrorMsg: "slot full",
		Admission: &roundpb.ServiceAdmission{
			Code:        roundpb.AdmissionCode_ADMISSION_FULL,
			OperationId: append([]byte{1}, make([]byte, 31)...),
			Mode:        roundpb.ServiceMode_SERVICE_SCHEDULED,
			NextSlot: &roundpb.SlotOpportunity{
				ScheduleVersion: 3,
				SlotIndex:       10,
			},
		},
	}
	var event BoardingFailed
	require.NoError(t, event.FromProto(pb))
	require.Equal(t, pb.Admission, event.Admission)
	pb.Admission.OperationId[0] = 2
	pb.Admission.NextSlot.SlotIndex = 11
	require.Equal(t, byte(1), event.Admission.OperationId[0])
	require.Equal(t, uint64(10), event.Admission.NextSlot.SlotIndex)
	pb.Admission.Code = roundpb.AdmissionCode_ADMISSION_ACCEPTED
	require.Error(t, event.FromProto(pb))
}

// TestServiceRejectionRouting isolates concurrent pending operations and
// refuses to send an unknown operation's rejection to an unrelated pending
// round.
func TestServiceRejectionRouting(t *testing.T) {
	h := newActorTestHarness(t)
	var targets []*RoundFSM
	for _, id := range []byte{1, 2} {
		key, err := NewTempRoundKey()
		require.NoError(t, err)
		state := &IntentSentState{Intents: Intents{
			Service: &types.ServiceRequest{
				Mode: types.ServiceScheduled,
				OperationID: [32]byte{
					id,
				},
			},
		}}
		machine := protofsm.NewInlineStateMachine(ClientStateMachineCfg{
			Logger:        h.actor.log,
			ErrorReporter: newLoggerErrorReporter(h.actor.log),
			InitialState:  state, Env: h.actor.env,
		})
		machine.Start(h.ctx)
		t.Cleanup(machine.Stop)
		target := &RoundFSM{FSM: &machine, Key: key}
		h.actor.rounds[RoundKeyStr(key.KeyString())] = target
		targets = append(targets, target)
	}
	for i, id := range []byte{1, 2, 3} {
		operationID := [32]byte{id}
		event := &BoardingFailed{Admission: &roundpb.ServiceAdmission{
			Code:        roundpb.AdmissionCode_ADMISSION_FULL,
			OperationId: operationID[:],
			Mode:        roundpb.ServiceMode_SERVICE_SCHEDULED,
		}}
		target, result, failed := h.actor.routeServerMessageToPending(
			h.ctx, event,
		)
		if id == 3 {
			require.True(t, failed)
			require.ErrorIs(t, result.Err(), ErrNoPendingRound)
			require.Nil(t, target)

			continue
		}
		require.False(t, failed)
		require.Same(t, targets[i], target)
	}
}

// TestServiceFallbackAuthorization permits one switch without releasing local
// inputs and rejects timeout, expiry, and uncertain-ownership alternatives.
func TestServiceFallbackAuthorization(t *testing.T) {
	request := &types.ServiceRequest{
		OperationID: [32]byte{
			1,
		}, Mode: types.ServiceScheduled,
		ScheduleVersion: 1, SlotIndex: 2, AllowFallback: true,
		FeeLimitSat: 100, ExpiresAtUnix: uint64(
			time.Now().Add(time.Hour).Unix(),
		),
	}
	state := &IntentSentState{Intents: Intents{Service: request}}
	failure := &BoardingFailed{Admission: &roundpb.ServiceAdmission{
		OperationId: request.OperationID[:],
		Mode:        roundpb.ServiceMode_SERVICE_SCHEDULED,
		Code:        roundpb.AdmissionCode_ADMISSION_FULL,
	}}
	tr := state.fallbackAfterNonAdmission(failure, time.Now())
	require.NotNil(t, tr)
	next, ok := tr.NextState.(*PendingRoundAssembly)
	require.True(t, ok)
	require.Equal(t, types.ServiceImmediate, next.Service.Mode)
	require.Equal(t, request.OperationID, next.Service.OperationID)
	require.Equal(t, request.ExpiresAtUnix, next.Service.ExpiresAtUnix)
	require.Equal(t, request.FeeLimitSat, next.Service.FeeLimitSat)
	require.Zero(t, next.Service.ScheduleVersion)
	require.Zero(t, next.Service.SlotIndex)
	require.Equal(t, types.ServiceScheduled, request.Mode)
	emitted := tr.NewEvents.UnwrapOrFail(t)
	require.Empty(
		t, emitted.Outbox, "fallback cannot release reserved inputs",
	)
	require.Len(t, emitted.InternalEvent, 1)
	require.Nil(
		t,
		state.fallbackAfterNonAdmission(
			&BoardingFailed{}, time.Now(),
		),
	)
	for _, code := range []roundpb.AdmissionCode{
		roundpb.AdmissionCode_ADMISSION_ACCEPTED,
		roundpb.AdmissionCode_ADMISSION_UNCERTAIN,
		roundpb.AdmissionCode_ADMISSION_CONFLICT,
		roundpb.AdmissionCode_ADMISSION_UNAUTHORIZED,
		roundpb.AdmissionCode_ADMISSION_EXPIRED,
	} {
		failure.Admission.Code = code
		require.Nil(
			t,
			state.fallbackAfterNonAdmission(
				failure, time.Now(),
			),
		)
	}
	failure.Admission.Code = roundpb.AdmissionCode_ADMISSION_FULL
	request.AllowFallback = false
	require.Nil(t, state.fallbackAfterNonAdmission(failure, time.Now()))
	request.AllowFallback = true
	state.AdmittedRoundID = RoundID{1}
	require.Nil(t, state.fallbackAfterNonAdmission(failure, time.Now()))
	state.AdmittedRoundID = RoundID{}
	request.ExpiresAtUnix = 1
	require.Nil(t, state.fallbackAfterNonAdmission(failure, time.Now()))
}

// TestServiceDeadlineFencesSigning checks the clock at protocol progress, so a
// queued timer cannot permit a late signing event to advance the state machine.
func TestServiceDeadlineFencesSigning(t *testing.T) {
	env := &ClientEnvironment{Log: btclog.Disabled,
		ParticipationDeadline: time.Now().Add(-time.Second),
	}
	state := &NoncesAggregatedState{RoundID: RoundID{1}}
	transition, err := state.ProcessEvent(
		t.Context(), &GeneratePartialSigs{}, env,
	)
	require.NoError(t, err)
	failed, ok := transition.NextState.(*ClientFailedState)
	require.True(t, ok)
	require.Contains(t, failed.Reason, "participation deadline")
	emitted := transition.NewEvents.UnwrapOr(ClientEmittedEvent{})
	_, signed := findOutbox[*SubmitPartialSigRequest](emitted.Outbox)
	require.False(t, signed)
}
