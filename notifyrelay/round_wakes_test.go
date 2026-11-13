package notifyrelay

import (
	"bytes"
	"encoding/base64"
	"testing"
	"time"

	pb "github.com/lightninglabs/wavelength/mailbox/pb"
	"github.com/lightninglabs/wavelength/notification"
	"github.com/lightninglabs/wavelength/notifyrelay/relaydb/sqlc"
	"github.com/stretchr/testify/require"
)

// roundWakeHarness enrolls an existing local route with explicit round consent.
func roundWakeHarness(t *testing.T) (*sqlAuthHarness, *feedIngress, string) {
	t.Helper()
	config := testSourceConfig(t, "operator")
	config.EventClasses = notification.FeedClassWake |
		notification.FeedClassRound
	h := sourceHarness(t, config)
	id := h.sqlSubscription(t, "wallet")
	require.NoError(
		t,
		h.bindTestFeed(
			t, id, FeedBindingRequest{
				SourceID: config.ID,
				FeedToken: base64.RawURLEncoding.EncodeToString(
					bytes.Repeat(
						[]byte{9}, 32,
					),
				),
				EventClasses: config.EventClasses,
				ExpiresAtUnix: h.now.
					Add(24 * time.Hour).
					Unix(),
			},
		),
	)

	return h, &feedIngress{
		store:  h.store,
		source: h.store.sources[config.ID],
	}, id
}

// admitRoundWake uses the real feed binding/generation and job transaction.
func admitRoundWake(t *testing.T, h *sqlAuthHarness, f *feedIngress, id string,
	generation uint64, deadline time.Time) {

	t.Helper()
	event := &pb.WakeFeedEvent{
		GrantId: id, FeedToken: bytes.Repeat([]byte{9}, 32),
		Generation: generation,
		EventClasses: notification.FeedClassWake |
			notification.FeedClassRound,
		ExpiresAtUnix:     h.now.Add(time.Hour).Unix(),
		RoundDeadlineUnix: deadline.Unix(),
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

// TestRoundWakeIgnoresMaintenanceQuietPeriod proves a prior accepted generic
// wake cannot defer signing, while routes still share a bounded urgent budget.
func TestRoundWakeIgnoresMaintenanceQuietPeriod(t *testing.T) {
	h, f, id := roundWakeHarness(t)
	h.sqlEnqueue(t, id, ScopeWake, "")
	ordinary, err := h.store.claim(t.Context(), h.now)
	require.NoError(t, err)
	require.NotNil(t, ordinary)
	require.NoError(
		t,
		h.store.complete(
			t.Context(), *ordinary, ProviderResult{
				Outcome: OutcomeAccepted,
			},
			h.now,
		),
	)
	admitRoundWake(t, h, f, id, 1, h.now.Add(time.Minute))
	signing, err := h.store.claim(t.Context(), h.now)
	require.NoError(t, err)
	require.NotNil(t, signing)
	require.Equal(t, ScopeRound, signing.job.Scope)
	require.Equal(t, ScopeWake, signing.Scope, "existing silent host route")
	payload, err := notificationPayload(*signing)
	require.NoError(t, err)
	require.NotContains(t, string(payload), "alert")
	admitRoundWake(t, h, f, id, 2, h.now.Add(time.Minute))
	require.NoError(
		t,
		h.store.complete(
			t.Context(), *signing, ProviderResult{
				Outcome: OutcomeAccepted,
			},
			h.now,
		),
	)
	blocked, err := h.store.claim(t.Context(), h.now)
	require.NoError(t, err)
	require.Nil(t, blocked)
	peerDB := h.open()
	defer func() {
		require.NoError(t, peerDB.Close())
	}()
	peer, err := NewSQLStore(t.Context(), peerDB)
	require.NoError(t, err)
	next, err := peer.claim(t.Context(), h.now.Add(roundQuietPeriod))
	require.NoError(t, err)
	require.NotNil(
		t, next, "new generation survives completion and restart",
	)
}

// TestRoundWakeExpiredAndReplay consumes source progress without refreshing
// an expired or already committed urgent push obligation.
func TestRoundWakeExpiredAndReplay(t *testing.T) {
	h, f, id := roundWakeHarness(t)
	admitRoundWake(t, h, f, id, 1, h.now.Add(-time.Second))
	count, err := h.database.Queries.CountPendingJobs(t.Context())
	require.NoError(t, err)
	require.Zero(t, count)
	admitRoundWake(t, h, f, id, 2, h.now.Add(3*time.Second))
	admitRoundWake(t, h, f, id, 2, h.now.Add(time.Minute))
	job, err := h.database.Queries.GetJob(t.Context(), "round/"+id)
	require.NoError(t, err)
	require.EqualValues(t, 1, job.Generation)
	require.Equal(
		t, time.Unix(h.now.Add(3*time.Second).Unix(), 0).UnixNano(),
		job.ExpiresAt,
	)
	sent, err := h.store.claim(t.Context(), h.now.Add(4*time.Second))
	require.NoError(t, err)
	require.Nil(t, sent)
}

// TestScheduledRoundWakeWindowAndRestart proves the exact opening/deadline
// and atomic cutoff marker, including duplicate calls from another replica.
func TestScheduledRoundWakeWindowAndRestart(t *testing.T) {
	h, _, id := roundWakeHarness(t)
	opens := h.now.Add(time.Minute)
	deadline := opens.Add(50 * time.Second)
	cutoff := deadline.Add(2 * time.Second).Unix()
	require.NoError(
		t,
		h.store.scheduleRoundBindings(
			t.Context(),
			"operator", cutoff, opens, deadline, h.now,
		),
	)
	early, err := h.store.claim(t.Context(), opens.Add(-time.Nanosecond))
	require.NoError(t, err)
	require.Nil(t, early)
	peerDB := h.open()
	defer func() {
		require.NoError(t, peerDB.Close())
	}()
	peer, err := NewSQLStore(t.Context(), peerDB)
	require.NoError(t, err)
	require.NoError(
		t,
		peer.scheduleRoundBindings(
			t.Context(),
			"operator", cutoff, opens, deadline, h.now,
		),
	)
	job, err := h.database.Queries.GetJob(t.Context(), "schedule/"+id)
	require.NoError(t, err)
	require.EqualValues(t, 1, job.Generation)
	sent, err := peer.claim(t.Context(), opens)
	require.NoError(t, err)
	require.NotNil(t, sent)
	require.Equal(
		t, time.Unix(deadline.Unix(), 0).UnixNano(),
		sent.ExpiresAt.UnixNano(),
	)
	require.NoError(
		t,
		peer.complete(
			t.Context(), *sent, ProviderResult{
				Outcome: OutcomeAccepted,
			},
			opens,
		),
	)
	require.NoError(
		t,
		peer.scheduleRoundBindings(
			t.Context(),
			"operator", cutoff, opens, deadline, opens,
		),
	)
	count, err := h.database.Queries.CountPendingJobs(t.Context())
	require.NoError(t, err)
	require.Zero(t, count)
}

// TestScheduleRequiresRoundConsent refuses both old grants and expired ones.
func TestScheduleRequiresRoundConsent(t *testing.T) {
	h, _, _ := ingressHarness(t)
	require.NoError(
		t,
		h.store.scheduleRoundBindings(
			t.Context(),
			"producer", h.now.Add(time.Minute).Unix(), h.now,
			h.now.Add(time.Minute), h.now,
		),
	)
	count, err := h.database.Queries.CountPendingJobs(t.Context())
	require.NoError(t, err)
	require.Zero(t, count)
}

// TestOrdinaryFeedCannotUpgradeRoundConsent prevents a publisher source policy
// update alone from silently enrolling legacy wallet grants in scheduled wakes.
func TestOrdinaryFeedCannotUpgradeRoundConsent(t *testing.T) {
	h, f, envelope := ingressHarness(t)
	f.source.config.EventClasses |= notification.FeedClassRound
	event := new(pb.WakeFeedEvent)
	require.NoError(t, envelope.Body.UnmarshalTo(event))
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
	binding, err := h.database.Queries.GetFeedBinding(
		t.Context(), sqlc.GetFeedBindingParams{
			SourceID:       f.source.config.ID,
			SubscriptionID: event.GrantId,
		},
	)
	require.NoError(t, err)
	require.Zero(
		t, uint32(binding.EventClasses)&notification.FeedClassRound,
	)
}

// TestSuccessiveRoundWakesHaveDistinctProviderIdentity preserves retry
// identity while preventing job deletion from reusing it for a later phase.
func TestSuccessiveRoundWakesHaveDistinctProviderIdentity(t *testing.T) {
	h, f, id := roundWakeHarness(t)
	admitRoundWake(t, h, f, id, 1, h.now.Add(time.Minute))
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
	h.now = h.now.Add(roundQuietPeriod)
	admitRoundWake(t, h, f, id, 2, h.now.Add(time.Minute))
	second, err := h.store.claim(t.Context(), h.now)
	require.NoError(t, err)
	require.NotNil(t, second)
	require.EqualValues(t, 1, first.job.Generation)
	require.EqualValues(t, 1, second.job.Generation)
	require.NotEqual(
		t, providerRequestID(*first), providerRequestID(*second),
	)
	requestID := providerRequestID(*second)
	require.NoError(
		t,
		h.store.complete(
			t.Context(), *second, ProviderResult{
				Outcome: OutcomeRetry,
			},
			h.now,
		),
	)
	retried, err := h.store.claim(t.Context(), h.now.Add(roundQuietPeriod))
	require.NoError(t, err)
	require.NotNil(t, retried)
	require.Equal(t, requestID, providerRequestID(*retried))
}
