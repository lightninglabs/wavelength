package round

import (
	"testing"
	"time"

	"github.com/lightninglabs/wavelength/lib/batchschedule"
	"github.com/lightninglabs/wavelength/lib/types"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// scheduledRefreshHarness prepares a real refresh transition inside an open
// slot, with an operator clock that may differ from the local clock.
func scheduledRefreshHarness(t *testing.T, offset time.Duration,
	grace time.Duration, scheduled bool) (*boardingTestHarness, *time.Time,
	time.Time) {

	t.Helper()

	h := newTestHarness(t)
	cutoff := testAnchor.Add(20 * time.Minute)
	now := cutoff.Add(-5*time.Minute + 4*time.Second).Add(-offset)
	h.env.Now = func() time.Time { return now }
	h.env.RegistrationTimeout = grace
	h.env.RegistrationWakeJitter = time.Second
	if scheduled {
		schedule, err := batchschedule.New(
			testAnchor, 20*time.Minute, 5*time.Minute,
		)
		require.NoError(t, err)
		published := publishedTestSchedule(t, schedule, cutoff)
		h.env.OperatorTerms.BatchSchedule = fn.Some(
			published.WithClockOffset(offset),
		)
	}

	intent := h.newTestBoardingIntent()
	request := h.newTestVTXORequestForIntent(intent)
	op := regTimeoutOutpoint(0x41, 0)
	h.vtxoStore.On("GetVTXO", mock.Anything, op).Return(
		&ClientVTXO{Outpoint: op, Amount: request.Amount}, nil,
	)
	h.withState(&PendingRoundAssembly{
		Forfeits: []types.ForfeitRequest{mkForfeit(op, request.Amount)},
		VTXOs:    []types.VTXORequest{request},
	})

	return h, &now, cutoff.Add(-offset)
}

// TestScheduledRegistrationTimeout keeps the admission watermark reachable
// when a phone sleeps until the scheduled quote, without changing event-driven
// or disabled registration timeouts.
func TestScheduledRegistrationTimeout(t *testing.T) {
	for _, tc := range []struct {
		name      string
		scheduled bool
		offset    time.Duration
		grace     time.Duration
	}{
		{
			"scheduled",
			true,
			0,
			time.Minute,
		},
		{
			"operator-ahead",
			true,
			30 * time.Second,
			time.Minute,
		},
		{
			"operator-behind",
			true,
			-30 * time.Second,
			time.Minute,
		},
		{
			"event-driven",
			false,
			0,
			time.Minute,
		},
		{
			"disabled",
			true,
			0,
			-time.Second,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, now, cutoff := scheduledRefreshHarness(
				t, tc.offset, tc.grace, tc.scheduled,
			)
			tr, err := h.sendEvent(&IntentRequested{})
			require.NoError(t, err)
			assertStateType[*IntentSentState](h)
			outbox := tr.NewEvents.UnwrapOr(
				ClientEmittedEvent{},
			).Outbox
			_, ok := findOutbox[*JoinRoundRequest](outbox)
			require.True(t, ok)
			timer, armed := findOutbox[*StartTimeoutReq](outbox)
			if tc.grace <= 0 {
				require.False(t, armed)

				return
			}
			require.True(t, armed)
			require.Equal(t, TimeoutPhaseRegistration, timer.Phase)
			deadline := now.Add(timer.Duration)
			if tc.scheduled {
				require.Equal(t, cutoff.Add(tc.grace), deadline)
			} else {
				require.Equal(t, now.Add(tc.grace), deadline)
			}
		})
	}
}

// TestScheduledRegistrationTimerAfterResume preserves the original timer
// through its durable codec after sleeping beyond the old one-minute bound.
func TestScheduledRegistrationTimerAfterResume(t *testing.T) {
	h, now, cutoff := scheduledRefreshHarness(
		t, 30*time.Second, time.Minute, true,
	)
	tr, err := h.sendEvent(&IntentRequested{})
	require.NoError(t, err)
	outbox := tr.NewEvents.UnwrapOr(ClientEmittedEvent{}).Outbox
	timer, ok := findOutbox[*StartTimeoutReq](outbox)
	require.True(t, ok)
	effect, err := newDurableClientEffect(timer, *now)
	require.NoError(t, err)
	codec := newDurableClientCodec()
	raw, err := codec.Encode(effect)
	require.NoError(t, err)

	// Resume with the acknowledgement still awaiting its native turn.
	*now = now.Add(90 * time.Second)
	decoded, err := codec.Decode(raw)
	require.NoError(t, err)
	restored, ok := decoded.(*durableClientEffect)
	require.True(t, ok)
	message, err := restored.timerMessage(*now)
	require.NoError(t, err)
	remaining, ok := message.(*StartTimeoutReq)
	require.True(t, ok)
	require.Positive(t, remaining.Duration)
	require.Equal(t, cutoff.Add(time.Minute), now.Add(remaining.Duration))

}
