package round

import (
	"testing"
	"time"

	"github.com/lightninglabs/wavelength/lib/batchschedule"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/stretchr/testify/require"
)

// TestScheduledRegistrationWindow checks waiting, exact opening, and missed
// cutoff without invoking a signer or sending a premature join request.
func TestScheduledRegistrationWindow(t *testing.T) {
	t.Parallel()
	anchor := time.Unix(1800000000, 0)
	schedule, err := batchschedule.New(anchor, time.Hour, time.Minute)
	require.NoError(t, err)
	now := anchor.Add(10 * time.Minute)
	env := &ClientEnvironment{
		OperatorTerms: &types.OperatorTerms{
			BatchSchedule: schedule,
		},
		Now:      func() time.Time { return now },
		RoundKey: "pending",
	}
	s := &PendingRoundAssembly{}
	tr, err := s.waitForScheduledSlot(env)
	require.NoError(t, err)
	require.Same(t, s, tr.NextState)
	outbox := tr.NewEvents.UnwrapOr(ClientEmittedEvent{}).Outbox
	wakeup, ok := outbox[0].(*StartTimeoutReq)
	require.True(t, ok)
	require.Equal(t, 49*time.Minute, wakeup.Duration)
	require.Equal(t, TimeoutPhaseScheduledRegistration, wakeup.Phase)
	selection := *env.scheduledSlot
	now = anchor.Add(59 * time.Minute)
	tr, err = s.waitForScheduledSlot(env)
	require.NoError(t, err)
	require.Nil(t, tr)
	require.Equal(t, selection, *env.scheduledSlot)
	now = anchor.Add(time.Hour)
	_, err = s.waitForScheduledSlot(env)
	require.ErrorContains(t, err, "closed")
	require.Equal(t, selection, *env.scheduledSlot)
	tr, err = s.waitForScheduledSlot(&ClientEnvironment{})
	require.NoError(t, err)
	require.Nil(t, tr)
}

// TestScheduledRegistrationCursor honors a discovered restart fence, but a
// stale discovery snapshot does not hold the client on an expired slot.
func TestScheduledRegistrationCursor(t *testing.T) {
	t.Parallel()
	anchor := time.Unix(1800000000, 0)
	schedule, err := batchschedule.New(anchor, time.Hour, time.Minute)
	require.NoError(t, err)
	now := anchor.Add(10 * time.Minute)
	terms := &types.OperatorTerms{
		BatchSchedule:   schedule,
		NextBatchCutoff: anchor.Add(2 * time.Hour),
	}
	env := &ClientEnvironment{
		OperatorTerms: terms,
		Now:           func() time.Time { return now },
	}
	s := &PendingRoundAssembly{}
	_, err = s.waitForScheduledSlot(env)
	require.NoError(t, err)
	require.Equal(
		t,
		uint64(
			terms.NextBatchCutoff.Unix(),
		),
		env.scheduledSlot.CutoffUnix,
	)
	now = anchor.Add(3 * time.Hour)
	env.scheduledSlot = nil
	_, err = s.waitForScheduledSlot(env)
	require.NoError(t, err)
	require.Equal(
		t,
		uint64(
			anchor.Add(4*time.Hour).Unix(),
		),
		env.scheduledSlot.CutoffUnix,
	)
}
