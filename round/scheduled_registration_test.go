package round

import (
	"context"
	"testing"
	"time"

	"github.com/lightninglabs/wavelength/arkrpc"
	"github.com/lightninglabs/wavelength/lib/batchschedule"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/stretchr/testify/mock"
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
			BatchSchedule: publishedTestSchedule(
				t, schedule, anchor.Add(time.Hour),
			),
		},
		Now:      func() time.Time { return now },
		RoundKey: "pending",
	}
	s := &PendingRoundAssembly{}
	tr, err := s.waitForScheduledSlot(t.Context(), env)
	require.NoError(t, err)
	require.Same(t, s, tr.NextState)
	outbox := tr.NewEvents.UnwrapOr(ClientEmittedEvent{}).Outbox
	wakeup, ok := outbox[0].(*StartTimeoutReq)
	require.True(t, ok)
	require.Equal(t, 49*time.Minute, wakeup.Duration)
	require.Equal(t, TimeoutPhaseScheduledRegistration, wakeup.Phase)
	selection := *env.scheduledSlot
	now = anchor.Add(59 * time.Minute)
	tr, err = s.waitForScheduledSlot(t.Context(), env)
	require.NoError(t, err)
	require.Nil(t, tr)
	require.Equal(t, selection, *env.scheduledSlot)
	now = anchor.Add(time.Hour)
	_, err = s.waitForScheduledSlot(t.Context(), env)
	require.ErrorContains(t, err, "closed")
	require.Equal(t, selection, *env.scheduledSlot)
	tr, err = s.waitForScheduledSlot(t.Context(), &ClientEnvironment{})
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
		BatchSchedule: publishedTestSchedule(
			t, schedule, anchor.Add(2*time.Hour),
		),
	}
	env := &ClientEnvironment{
		OperatorTerms: terms,
		Now:           func() time.Time { return now },
	}
	s := &PendingRoundAssembly{}
	_, err = s.waitForScheduledSlot(t.Context(), env)
	require.NoError(t, err)
	require.True(t, env.scheduledSlot.Cutoff.Equal(anchor.Add(2*time.Hour)))
	now = anchor.Add(3 * time.Hour)
	env.scheduledSlot = nil
	_, err = s.waitForScheduledSlot(t.Context(), env)
	require.NoError(t, err)
	require.True(t, env.scheduledSlot.Cutoff.Equal(anchor.Add(4*time.Hour)))
}

// TestScheduledRegistrationLatestTerms pins refreshed policy at selection and
// keeps that policy stable even when the shared cache changes while waiting.
func TestScheduledRegistrationLatestTerms(t *testing.T) {
	t.Parallel()
	anchor := time.Unix(1800000000, 0)
	schedule, err := batchschedule.New(anchor, time.Hour, time.Minute)
	require.NoError(t, err)
	latest := &types.OperatorTerms{
		BatchSchedule: publishedTestSchedule(
			t, schedule, anchor.Add(2*time.Hour),
		),
	}
	now := anchor.Add(10 * time.Minute)
	env := &ClientEnvironment{
		OperatorTerms: &types.OperatorTerms{},
		OperatorTermsSource: func(context.Context) (
			*types.OperatorTerms, error) {

			return latest, nil
		},
		Now: func() time.Time { return now },
	}
	s := &PendingRoundAssembly{}
	_, err = s.waitForScheduledSlot(t.Context(), env)
	require.NoError(t, err)
	require.Same(t, latest, env.OperatorTerms)
	require.True(t, env.scheduledSlot.Cutoff.Equal(anchor.Add(2*time.Hour)))
	selected := env.OperatorTerms
	latest = &types.OperatorTerms{}
	now = anchor.Add(119 * time.Minute)
	tr, err := s.waitForScheduledSlot(t.Context(), env)
	require.NoError(t, err)
	require.Nil(t, tr)
	require.Same(t, selected, env.OperatorTerms)
}

// TestScheduledPreparationCrossesCutoff exercises the real assembly transition
// with a wallet key derivation that consumes the remaining registration time.
func TestScheduledPreparationCrossesCutoff(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	anchor := time.Unix(1800000000, 0)
	schedule, err := batchschedule.New(anchor, time.Hour, time.Minute)
	require.NoError(t, err)
	now := anchor.Add(59 * time.Minute)
	h.env.Now = func() time.Time { return now }
	h.env.OperatorTerms.BatchSchedule = publishedTestSchedule(
		t, schedule, anchor.Add(time.Hour),
	)
	for _, call := range h.wallet.ExpectedCalls {
		if call.Method == "DeriveNextKey" {
			call.Run(
				func(_ mock.Arguments) {
					now = anchor.Add(time.Hour)
				},
			)
		}
	}
	intent := h.newTestBoardingIntent()
	h.withState(
		&PendingRoundAssembly{
			Boarding: []BoardingIntent{intent},
			VTXOs: []types.VTXORequest{
				h.newTestVTXORequestForIntent(intent),
			},
		},
	)
	tr, err := h.sendEvent(&IntentRequested{})
	require.NoError(t, err)
	require.IsType(t, &ClientFailedState{}, tr.NextState)
	for _, msg := range tr.NewEvents.UnwrapOr(ClientEmittedEvent{}).Outbox {
		_, isJoin := msg.(*JoinRoundRequest)
		require.False(t, isJoin)
	}
}

// publishedTestSchedule constructs the same bounded discovery used by
// operators.
func publishedTestSchedule(t *testing.T, schedule *batchschedule.Schedule,
	cutoff time.Time) *batchschedule.Published {

	t.Helper()
	p, err := arkrpc.ParseBatchSchedule(
		arkrpc.BatchScheduleToProto(
			schedule, cutoff.Add(-time.Hour), cutoff,
		),
	)
	require.NoError(t, err)

	return p
}

// TestScheduledRegistrationExhausted never extrapolates an unpublished cutoff.
func TestScheduledRegistrationExhausted(t *testing.T) {
	t.Parallel()
	now := time.Unix(1800000000, 0)
	p, err := batchschedule.NewPublished([32]byte{1}, []batchschedule.Slot{
		{Opens: now.Add(-time.Minute), Cutoff: now},
	})
	require.NoError(t, err)
	env := &ClientEnvironment{
		OperatorTerms: &types.OperatorTerms{
			BatchSchedule: p,
		},
		Now: func() time.Time { return now },
	}
	s := &PendingRoundAssembly{}
	_, err = s.waitForScheduledSlot(t.Context(), env)
	require.ErrorIs(t, err, batchschedule.ErrScheduleExhausted)
	require.Nil(t, env.scheduledSlot)

	// A new discovery can supply a nonperiodic opportunity after the old
	// horizon. Use its explicit opening, not a derived registration window.
	p, err = batchschedule.NewPublished([32]byte{2}, []batchschedule.Slot{
		{Opens: now.Add(17 * time.Minute), Cutoff: now.Add(
			19 * time.Minute,
		)},
	})
	require.NoError(t, err)
	calls := 0
	env.OperatorTermsSource = func(ctx context.Context) (
		*types.OperatorTerms, error) {

		_, bounded := ctx.Deadline()
		require.True(t, bounded)
		calls++

		return &types.OperatorTerms{BatchSchedule: p}, nil
	}
	tr, err := s.waitForScheduledSlot(t.Context(), env)
	require.NoError(t, err)
	out := tr.NewEvents.UnwrapOr(ClientEmittedEvent{}).Outbox
	wakeup, ok := out[0].(*StartTimeoutReq)
	require.True(t, ok)
	require.Equal(t, 17*time.Minute, wakeup.Duration)
	require.True(t, env.scheduledSlot.Cutoff.Equal(now.Add(19*time.Minute)))
	now = now.Add(17 * time.Minute)
	tr, err = s.waitForScheduledSlot(t.Context(), env)
	require.NoError(t, err)
	require.Nil(t, tr)
	require.Equal(t, 1, calls)
}

// TestScheduledDiscoveryFailure rejects unavailable discovery without using
// old cached opportunities or silently falling back to legacy registration.
func TestScheduledDiscoveryFailure(t *testing.T) {
	t.Parallel()
	env := &ClientEnvironment{
		OperatorTermsSource: func(context.Context) (
			*types.OperatorTerms, error) {

			return nil, context.DeadlineExceeded
		},
	}
	state := &PendingRoundAssembly{}
	_, err := state.waitForScheduledSlot(t.Context(), env)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Nil(t, env.scheduledSlot)
}
