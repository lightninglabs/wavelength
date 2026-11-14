package notifyrelay

import (
	"bytes"
	"testing"
	"time"

	pb "github.com/lightninglabs/wavelength/mailbox/pb"
	"github.com/lightninglabs/wavelength/notification"
	"github.com/lightninglabs/wavelength/notifyrelay/relaydb/sqlc"
	"github.com/stretchr/testify/require"
)

// TestWorkWakeBurstKeepsProgressPrompt checks actual feed replay and coalescing
// across accepted sends. New work waits seconds, never a maintenance interval.
func TestWorkWakeBurstKeepsProgressPrompt(t *testing.T) {
	h, f, id := roundWakeHarness(t)
	admit := func(generation uint64) {
		event := &pb.WakeFeedEvent{
			GrantId: id, FeedToken: bytes.Repeat([]byte{9}, 32),
			Generation:    generation,
			EventClasses:  notification.FeedClassWake,
			ExpiresAtUnix: h.now.Add(time.Hour).Unix(),
		}
		require.NoError(
			t,
			h.database.Write(
				t.Context(),
				func(q *sqlc.Queries) error {
					return f.admitEvent(
						t.Context(), q, event, h.now,
					)
				},
			),
		)
	}
	admit(1)
	first, err := h.store.claim(t.Context(), h.now)
	require.NoError(t, err)
	require.NotNil(t, first)
	require.NoError(
		t,
		h.store.complete(
			t.Context(), *first, ProviderResult{
				Outcome: OutcomeAccepted,
			},
			h.now,
		),
	)
	h.now = h.now.Add(time.Second)
	admit(2)
	admit(2)
	job, err := h.database.Queries.GetJob(t.Context(), "wake/"+id)
	require.NoError(t, err)
	require.EqualValues(
		t, 1, job.Generation, "same source generation is a replay",
	)
	admit(3)
	job, err = h.database.Queries.GetJob(t.Context(), "wake/"+id)
	require.NoError(t, err)
	require.EqualValues(t, 2, job.Generation)
	blocked, err := h.store.claim(t.Context(), h.now.Add(3*time.Second))
	require.NoError(t, err)
	require.Nil(t, blocked, "burst remains coalesced inside five seconds")
	next, err := h.store.claim(t.Context(), h.now.Add(5*time.Second))
	require.NoError(t, err)
	require.NotNil(t, next, "new financial work cannot wait twenty minutes")
	require.EqualValues(t, 2, next.job.Generation)
	require.NoError(
		t,
		h.store.complete(
			t.Context(), *next, ProviderResult{
				Outcome: OutcomeAccepted,
			},
			h.now.Add(5*time.Second),
		),
	)
	admit(3)
	count, err := h.database.Queries.CountPendingJobs(t.Context())
	require.NoError(t, err)
	require.Zero(
		t, count, "completed source replay cannot create another wake",
	)
}

// TestScheduleWakeIgnoresOldQuietAndAlertBudget keeps each consented slot
// eligible, including after a late accepted send or an old replica's quiet
// deadline. The cutoff marker still suppresses a duplicate schedule step.
func TestScheduleWakeIgnoresOldQuietAndAlertBudget(t *testing.T) {
	h, _, id := roundWakeHarness(t)
	require.NoError(
		t,
		h.database.Write(
			t.Context(),
			func(q *sqlc.Queries) error {
				owner, err := loadSQLInstallation(
					t.Context(), q, "device",
				)
				if err != nil {
					return err
				}
				owner.NextScheduleAt = h.now.
					Add(time.Hour).
					UnixNano()
				owner.NextAlertAt = h.now.
					Add(time.Hour).
					UnixNano()

				return saveSQLInstallation(
					t.Context(), q, owner,
				)
			},
		),
	)
	opens := h.now.Add(time.Minute)
	for range 2 {
		deadline := opens.Add(time.Minute)
		cutoff := deadline.Add(2 * time.Second).Unix()
		require.NoError(
			t,
			h.store.scheduleRoundBindings(
				t.Context(),
				"operator", cutoff, opens, deadline, opens,
			),
		)
		sent, err := h.store.claim(t.Context(), opens)
		require.NoError(t, err)
		require.NotNil(t, sent)
		require.Equal(t, ScopeSchedule, sent.job.Scope)
		require.NoError(
			t,
			h.store.complete(
				t.Context(), *sent, ProviderResult{
					Outcome: OutcomeAccepted,
				},
				deadline.Add(-time.Millisecond),
			),
		)
		require.NoError(
			t,
			h.store.scheduleRoundBindings(
				t.Context(),
				"operator", cutoff, opens, deadline, opens,
			),
		)
		count, err := h.database.Queries.CountPendingJobs(t.Context())
		require.NoError(t, err)
		require.Zero(
			t, count, "same cutoff cannot requeue an accepted job",
		)
		opens = deadline.Add(time.Minute)
	}
	jobCount, err := h.database.Queries.CountPendingJobs(t.Context())
	require.NoError(t, err)
	require.Zero(t, jobCount)
	sub, err := h.database.Queries.GetSubscription(t.Context(), id)
	require.NoError(t, err)
	require.NotEmpty(t, sub.RouteID)
}
