package round

import (
	"context"
	"testing"
	"time"

	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/lightninglabs/wavelength/rpc/roundpb"
	"github.com/stretchr/testify/require"
)

// TestServiceDeadlineUsesEnvironmentClock checks both sides of a fixed
// deadline.
func TestServiceDeadlineUsesEnvironmentClock(t *testing.T) {
	now := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	expiry := now.Add(2 * time.Second)
	env := &ClientEnvironment{
		Now:                    func() time.Time { return now },
		ParticipationDeadline:  expiry,
		StatusReconcileTimeout: 5 * time.Second,
	}
	request := &types.ServiceRequest{
		OperationID: [32]byte{
			1,
		}, Mode: types.ServiceScheduled,
		AllowFallback: true, ExpiresAtUnix: uint64(expiry.Unix()),
	}
	pending := &IntentSentState{Intents: Intents{Service: request}}
	rejection := &BoardingFailed{Admission: &roundpb.ServiceAdmission{
		OperationId: request.OperationID[:],
		Mode:        roundpb.ServiceMode_SERVICE_SCHEDULED,
		Code:        roundpb.AdmissionCode_ADMISSION_FULL,
	}}
	require.NotNil(
		t,
		pending.fallbackAfterNonAdmission(
			rejection, env.now(),
		),
	)
	require.False(t, env.participationExpired())
	event := &QuoteAccepted{}
	require.Same(t, event, env.boundParticipationEvent(event))
	ctx, cancel := env.participationContext(t.Context())
	require.NoError(t, ctx.Err())
	cancel()

	state := &ServiceReconcileState{Intents: Intents{Service: request}}
	transition := state.probe(env)
	emitted := transition.NewEvents.UnwrapOr(ClientEmittedEvent{})
	require.Len(t, emitted.Outbox, 2)
	timer, ok := emitted.Outbox[1].(*StartTimeoutReq)
	require.True(t, ok)
	require.Equal(t, 2*time.Second, timer.Duration)

	now = expiry
	require.Nil(t, pending.fallbackAfterNonAdmission(rejection, env.now()))
	require.True(t, env.participationExpired())
	require.IsType(t, &BoardingFailed{}, env.boundParticipationEvent(event))
	expiredCtx, cancelExpired := env.participationContext(t.Context())
	defer cancelExpired()
	require.ErrorIs(t, expiredCtx.Err(), context.DeadlineExceeded)
	transition = state.probe(env)
	require.True(t, transition.NewEvents.IsNone())
	require.Equal(t, "ServiceDeferred", transition.NextState.String())
}

// TestParticipationTimeoutRearmsAfterClockRollback checks the actor callback,
// including the duration it hands to the timer, against the round clock.
func TestParticipationTimeoutRearmsAfterClockRollback(t *testing.T) {
	h := newActorTestHarness(t)
	now := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	deadline := now.Add(10 * time.Second)
	key, err := NewTempRoundKey()
	require.NoError(t, err)
	keyString := RoundKeyStr(key.KeyString())
	h.actor.rounds[keyString] = &RoundFSM{
		Key: key,
		env: &ClientEnvironment{
			Now: func() time.Time { return now },
		},
		Admission: &roundpb.ServiceAdmission{
			ParticipationDeadlineUnix: deadline.Unix(),
		},
	}
	var timers []*StartTimeoutReq
	h.actor.queueEffect = func(_ context.Context,
		effect ClientOutMsg) error {

		timer, ok := effect.(*StartTimeoutReq)
		require.True(t, ok)
		timers = append(timers, timer)

		return nil
	}
	callback := &TimeoutMsg{
		TimeoutID: makeTimeoutID(keyString, TimeoutPhaseParticipation),
	}
	require.NoError(t, h.actor.handleTimeout(t.Context(), callback).Err())
	require.Len(t, timers, 1)
	require.Equal(t, 10*time.Second, timers[0].Duration)
	now = now.Add(-time.Minute)
	require.NoError(t, h.actor.handleTimeout(t.Context(), callback).Err())
	require.Len(t, timers, 2)
	require.Equal(t, 70*time.Second, timers[1].Duration)
	require.Equal(t, keyString, timers[1].RoundKey)
	require.Equal(t, TimeoutPhaseParticipation, timers[1].Phase)
}
