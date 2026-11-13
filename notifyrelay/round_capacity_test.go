package notifyrelay

import (
	"fmt"
	"testing"
	"time"

	"github.com/lightninglabs/wavelength/notifyrelay/relaydb/sqlc"
	"github.com/stretchr/testify/require"
)

// fillRoundJobPool models valid per-installation route cardinality while
// filling one global delivery pool. The fixture bypasses only provider auth,
// whose authority is covered independently by the enrollment tests.
func fillRoundJobPool(t *testing.T, h *sqlAuthHarness, scope Scope) {
	t.Helper()
	owner, err := h.store.loadInstallation(t.Context(), "device")
	require.NoError(t, err)
	ctx := t.Context()
	expiry := h.now.Add(24 * time.Hour).Unix()
	nextAt := h.now.Add(time.Hour).UnixNano()
	jobExpiry := h.now.Add(2 * time.Hour).UnixNano()
	err = h.database.Write(ctx, func(q *sqlc.Queries) error {
		for i := range maxPendingJobs {
			if i%maxSubscriptions == 0 {
				owner.ID = fmt.Sprintf("capacity-device-%d",
					i/maxSubscriptions)
				if err := saveSQLInstallation(
					ctx, q, owner,
				); err != nil {
					return err
				}
			}
			id := fmt.Sprintf("capacity-route-%d", i)
			sub := sqlc.PutSubscriptionParams{
				ID: id, InstallationID: owner.ID, RouteID: id,
				AlertsEnabled: true,
				Presentation:  string(PresentationGeneric),
				ExpiresAt:     expiry, Revision: 1,
			}
			if _, err := q.PutSubscription(ctx, sub); err != nil {
				return err
			}
			job := sqlc.CoalesceJobParams{
				ID:             string(scope) + "/" + id,
				SubscriptionID: id,
				Scope:          string(scope),
				EventHash:      "fixture",
				NextAt:         nextAt,
				ExpiresAt:      jobExpiry,
			}
			if err := q.CoalesceJob(ctx, job); err != nil {
				return err
			}
		}

		return nil
	})
	require.NoError(t, err)
}

// TestFutureRoundSchedulesCannotBlockDelivery fills the future schedule pool
// and proves both signing and existing payment work can still be admitted.
func TestFutureRoundSchedulesCannotBlockDelivery(t *testing.T) {
	h, f, id := roundWakeHarness(t)
	fillRoundJobPool(t, h, ScopeSchedule)
	admitRoundWake(t, h, f, id, 1, h.now.Add(time.Minute))
	h.sqlEnqueue(t, id, ScopeAlert, "payment")
	sent, err := h.store.claim(t.Context(), h.now)
	require.NoError(t, err)
	require.NotNil(t, sent)
	require.Equal(t, ScopeRound, sent.job.Scope)
	for _, scope := range []Scope{ScopeSchedule, ScopeRound} {
		count, err := h.database.Queries.CountPendingJobsForScope(
			t.Context(), string(scope),
		)
		require.NoError(t, err)
		if scope == ScopeSchedule {
			require.EqualValues(t, maxPendingJobs, count)
		}
		if scope == ScopeRound {
			require.EqualValues(t, 1, count)
		}
	}
	// The bound still applies to the saturated pool itself.
	err = h.database.Write(t.Context(), func(q *sqlc.Queries) error {
		sub, err := q.GetSubscription(t.Context(), id)
		if err != nil {
			return err
		}

		return enqueueSQLJobTx(
			t.Context(), q, sub, ScopeSchedule, "", h.now,
		)
	})
	require.ErrorIs(t, err, ErrCapacity)
}

// TestMaintenanceCapacityCannotBlockRoundSigning preserves a bounded urgent
// pool even when ordinary wake jobs are deferred behind their quiet period.
func TestMaintenanceCapacityCannotBlockRoundSigning(t *testing.T) {
	h, f, id := roundWakeHarness(t)
	fillRoundJobPool(t, h, ScopeWake)
	admitRoundWake(t, h, f, id, 1, h.now.Add(time.Minute))
	count, err := h.database.Queries.CountPendingJobsForScope(
		t.Context(), string(ScopeRound),
	)
	require.NoError(t, err)
	require.EqualValues(t, 1, count)
	// Old wake and alert permissions still share their original limit.
	err = h.database.Write(t.Context(), func(q *sqlc.Queries) error {
		sub, err := q.GetSubscription(t.Context(), id)
		if err != nil {
			return err
		}

		return enqueueSQLJobTx(
			t.Context(), q, sub, ScopeAlert, "another-payment",
			h.now,
		)
	})
	require.ErrorIs(t, err, ErrCapacity)
}

// TestNextScheduledSlotCannotInheritClaimedWindow proves a delayed provider
// result cannot make the next slot due before its future registration opens.
func TestNextScheduledSlotCannotInheritClaimedWindow(t *testing.T) {
	for _, expireClaim := range []bool{false, true} {
		t.Run(fmt.Sprint(expireClaim), func(t *testing.T) {
			h, _, id := roundWakeHarness(t)
			deadline := h.now.Add(10 * time.Second)
			cutoff := deadline.Add(2 * time.Second).Unix()
			require.NoError(
				t,
				h.store.scheduleRoundBindings(
					t.Context(),
					"operator", cutoff, h.now, deadline,
					h.now,
				),
			)
			old, err := h.store.claim(t.Context(), h.now)
			require.NoError(t, err)
			require.NotNil(t, old)
			h.now = h.now.Add(11 * time.Second)
			if expireClaim {
				h.now = h.now.Add(claimLifetime)
			}
			opens := h.now.Add(time.Hour)
			nextDeadline := opens.Add(time.Minute)
			nextCutoff := nextDeadline.Add(2 * time.Second).Unix()
			require.NoError(
				t,
				h.store.scheduleRoundBindings(
					t.Context(),
					"operator", nextCutoff, opens,
					nextDeadline, h.now,
				),
			)
			before, err := h.database.Queries.GetFeedBinding(
				t.Context(), sqlc.GetFeedBindingParams{
					SourceID:       "operator",
					SubscriptionID: id,
				},
			)
			require.NoError(t, err)
			if !expireClaim {
				require.Equal(t, cutoff, before.ScheduledCutoff)
			}
			require.NoError(
				t,
				h.store.complete(
					t.Context(), *old, ProviderResult{
						Outcome: OutcomeAccepted,
					},
					h.now,
				),
			)
			require.NoError(
				t,
				h.store.scheduleRoundBindings(
					t.Context(),
					"operator", nextCutoff, opens,
					nextDeadline, h.now,
				),
			)
			future, err := h.database.Queries.GetJob(
				t.Context(), "schedule/"+id,
			)
			require.NoError(t, err)
			require.Equal(t, opens.UnixNano(), future.NextAt)
			early, err := h.store.claim(
				t.Context(), opens.Add(-time.Second),
			)
			require.NoError(t, err)
			require.Nil(t, early)
			sent, err := h.store.claim(t.Context(), opens)
			require.NoError(t, err)
			require.NotNil(t, sent)
		})
	}
}
