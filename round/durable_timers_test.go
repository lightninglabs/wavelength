package round

import (
	"testing"
	"time"

	"github.com/lightninglabs/wavelength/timeout"
	"github.com/stretchr/testify/require"
)

// TestDurableTimerSnapshot preserves the deadline after an effect was
// delivered.
func TestDurableTimerSnapshot(t *testing.T) {
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	owner := &RoundClientActor{env: &ClientEnvironment{
		Now: func() time.Time { return now },
	}}
	key := RoundKeyStr("temp:key")
	phase := TimeoutPhaseRegistration
	id := makeTimeoutID(key, phase)
	_, err := owner.captureDurableEffect(&StartTimeoutReq{
		RoundKey: key, Phase: phase, Duration: time.Minute,
	})
	require.NoError(t, err)
	raw, err := owner.encodeActorSnapshot(t.Context())
	require.NoError(t, err)
	now = now.Add(45 * time.Second)
	snapshot, err := decodeActorSnapshot(raw, owner.env)
	require.NoError(t, err)
	require.Len(t, snapshot.pendingTimers, 1)
	restored, err := snapshot.pendingTimers[id].localMessage(now)
	require.NoError(t, err)
	timer, ok := restored.(*StartTimeoutReq)
	require.True(t, ok)
	require.Equal(t, 15*time.Second, timer.Duration)

	_, err = owner.captureDurableEffect(
		&CancelTimeoutReq{
			RoundKey: key,
			Phase:    phase,
		},
	)
	require.NoError(t, err)
	raw, err = owner.encodeActorSnapshot(t.Context())
	require.NoError(t, err)
	snapshot, err = decodeActorSnapshot(raw, owner.env)
	require.NoError(t, err)
	require.Empty(t, snapshot.pendingTimers)
}

// TestDurableTimerRouting rejects a timer recorded under another routing key.
func TestDurableTimerRouting(t *testing.T) {
	effect, err := newDurableClientEffect(&StartTimeoutReq{
		RoundKey: "temp:key", Phase: TimeoutPhaseRegistration,
		Duration: time.Minute,
	}, time.Now())
	require.NoError(t, err)
	raw, err := encodeDurableTimers(map[timeout.ID]*durableClientEffect{
		"another-timer": effect,
	})
	require.NoError(t, err)
	_, err = decodeDurableTimers(raw)
	require.ErrorContains(t, err, "timer routing key differs")
}

// TestDurableTimerGeneration ignores queued work from a superseded schedule.
func TestDurableTimerGeneration(t *testing.T) {
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	owner := &RoundClientActor{env: &ClientEnvironment{
		Now: func() time.Time { return now },
	}}
	request := &StartTimeoutReq{
		RoundKey: "temp:key", Phase: TimeoutPhaseRegistration,
		Duration: time.Minute,
	}
	first, err := owner.captureDurableEffect(request)
	require.NoError(t, err)
	_, _, firstDeadline, err := decodeDurableTimer(first.Payload)
	require.NoError(t, err)
	now = now.Add(time.Minute)
	second, err := owner.captureDurableEffect(request)
	require.NoError(t, err)
	current, err := owner.currentTimerEffect(first)
	require.NoError(t, err)
	require.False(t, current)
	current, err = owner.currentTimerEffect(second)
	require.NoError(t, err)
	require.True(t, current)
	staleCallback := &TimeoutMsg{
		TimeoutID: makeTimeoutID(request.RoundKey, request.Phase),
		deadline:  firstDeadline,
	}
	encoded, err := newDurableClientCommand(staleCallback)
	require.NoError(t, err)
	decoded, err := encoded.message(nil)
	require.NoError(t, err)
	callback, ok := decoded.(*TimeoutMsg)
	require.True(t, ok)
	current, err = owner.currentTimerCallback(callback)
	require.NoError(t, err)
	require.False(t, current)
	cancellation, err := owner.captureDurableEffect(&CancelTimeoutReq{
		RoundKey: request.RoundKey, Phase: request.Phase,
	})
	require.NoError(t, err)
	current, err = owner.currentTimerEffect(cancellation)
	require.NoError(t, err)
	require.True(t, current)
	current, err = owner.currentTimerEffect(second)
	require.NoError(t, err)
	require.False(t, current)
}
