package notifyrelay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	bolt "go.etcd.io/bbolt"
)

// TestCoalescingDuringDelivery proves accepting a newer event during a send
// cannot be erased when the older generation completes.
func TestCoalescingDuringDelivery(t *testing.T) {
	h := newRelayHarness(t, nil)
	sub := h.subscribe("wallet-route", true)
	ctx := t.Context()
	require.NoError(
		t,
		h.store.enqueue(
			ctx, sub.WakeCapability, ScopeWake, "first", h.now(),
		),
	)
	var deliveries []Delivery
	h.provider.send = func(_ context.Context, delivery Delivery) (
		ProviderResult, error) {

		deliveries = append(deliveries, delivery)
		if len(deliveries) == 1 {
			require.NoError(
				t,
				h.store.enqueue(
					ctx, sub.WakeCapability, ScopeWake,
					"second", h.now(),
				),
			)
		}

		return ProviderResult{Outcome: OutcomeAccepted}, nil
	}
	worked, err := h.server.DeliverOnce(ctx)
	require.NoError(t, err)
	require.True(t, worked)
	worked, err = h.server.DeliverOnce(ctx)
	require.NoError(t, err)
	require.False(
		t, worked, "accepted silent pushes observe the quiet period",
	)
	h.clock.Add(int64(wakeQuietPeriod))
	worked, err = h.server.DeliverOnce(ctx)
	require.NoError(t, err)
	require.True(t, worked)
	require.Len(t, deliveries, 2)
	require.Equal(t, deliveries[0].RouteID, deliveries[1].RouteID)
	require.NotEqual(t, deliveries[0].EventHash, deliveries[1].EventHash)
	// Replaying either event does not create another job.
	for _, event := range []string{"first", "second"} {
		require.NoError(
			t,
			h.store.enqueue(
				ctx, sub.WakeCapability, ScopeWake, event,
				h.now(),
			),
		)
	}
	h.clock.Add(int64(wakeQuietPeriod))
	worked, err = h.server.DeliverOnce(ctx)
	require.NoError(t, err)
	require.False(t, worked)
}

// TestDelayedInvalidTokenResponse proves an old provider response cannot
// disable a token rotated by an authenticated installation during the send.
func TestDelayedInvalidTokenResponse(t *testing.T) {
	h := newRelayHarness(t, nil)
	sub := h.subscribe("wallet-route", false)
	require.NoError(
		t,
		h.store.enqueue(
			t.Context(), sub.WakeCapability, ScopeWake, "payment",
			h.now(),
		),
	)
	claimed, err := h.store.claim(t.Context(), h.now())
	require.NoError(t, err)
	require.NotNil(t, claimed)
	response := h.manage(
		ManagementPayload{
			Action:      "rotate_token",
			DeviceToken: "eeff0011",
		},
	)
	require.Equal(t, 200, response.Code)
	require.NoError(
		t,
		h.store.complete(
			t.Context(), *claimed, ProviderResult{
				Outcome:   OutcomeInvalidToken,
				InvalidAt: h.now(),
			},
			h.now(),
		),
	)
	owner, err := h.store.loadInstallation(t.Context(), h.ownerID)
	require.NoError(t, err)
	require.False(t, owner.Disabled)
	require.Equal(t, "eeff0011", owner.DeviceToken)
	h.clock.Add(int64(10 * time.Second))
	h.provider.send = func(_ context.Context, delivery Delivery) (
		ProviderResult, error) {

		require.Equal(t, "eeff0011", delivery.DeviceToken)
		require.NotContains(
			t, fmt.Sprintf("%+v", delivery), delivery.DeviceToken,
		)
		raw, err := json.Marshal(delivery)
		require.NoError(t, err)
		require.NotContains(t, string(raw), delivery.DeviceToken)

		return ProviderResult{Outcome: OutcomeAccepted}, nil
	}
	worked, err := h.server.DeliverOnce(t.Context())
	require.NoError(t, err)
	require.True(t, worked)
}

// TestClaimRestartAndFencing reopens the same Bolt file after a leased job was
// interrupted. A stale completion cannot mutate the replacement claim.
func TestClaimRestartAndFencing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.db")
	store, err := OpenStore(path)
	require.NoError(t, err)
	h := newRelayHarness(t, store)
	sub := h.subscribe("wallet-route", false)
	require.NoError(
		t,
		store.enqueue(
			t.Context(), sub.WakeCapability, ScopeWake, "payment",
			h.now(),
		),
	)
	before, err := store.claim(t.Context(), h.now())
	require.NoError(t, err)
	require.NotNil(t, before)
	require.NoError(t, store.Close())
	store, err = OpenStore(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	h.clock.Add(int64(claimLifetime + time.Second))
	after, err := store.claim(t.Context(), h.now())
	require.NoError(t, err)
	require.NotNil(t, after)
	require.Equal(t, before.job.ID, after.job.ID)
	require.Equal(t, before.EventHash, after.EventHash)
	require.NotEqual(t, before.job.ClaimToken, after.job.ClaimToken)
	require.NoError(
		t,
		store.complete(
			t.Context(), *before, ProviderResult{
				Outcome: OutcomeInvalidToken,
			},
			h.now(),
		),
	)
	owner, err := store.loadInstallation(t.Context(), h.ownerID)
	require.NoError(t, err)
	require.False(t, owner.Disabled)
	require.NoError(
		t,
		store.complete(
			t.Context(), *after, ProviderResult{
				Outcome: OutcomeAccepted,
			},
			h.now(),
		),
	)
	require.NoError(
		t, store.db.View(func(tx *bolt.Tx) error {
			require.Zero(
				t, tx.Bucket([]byte(jobsBucket)).Stats().KeyN,
			)

			return nil
		}),
	)
}

// TestProviderRetryAndExpiry retains provider failures with the same event,
// then retires expired work instead of retrying indefinitely.
func TestProviderRetryAndExpiry(t *testing.T) {
	h := newRelayHarness(t, nil)
	sub := h.subscribe("wallet-route", true)
	require.NoError(
		t,
		h.store.enqueue(
			t.Context(), sub.AlertCapability, ScopeAlert, "payment",
			h.now(),
		),
	)
	var attempts []Delivery
	h.provider.send = func(_ context.Context, delivery Delivery) (
		ProviderResult, error) {

		attempts = append(attempts, delivery)

		return ProviderResult{Outcome: OutcomeRetry}, errors.New(
			"provider unavailable")
	}
	worked, err := h.server.DeliverOnce(t.Context())
	require.NoError(t, err)
	require.True(t, worked)
	worked, err = h.server.DeliverOnce(t.Context())
	require.NoError(t, err)
	require.False(t, worked)
	h.clock.Add(int64(10 * time.Second))
	worked, err = h.server.DeliverOnce(t.Context())
	require.NoError(t, err)
	require.True(t, worked)
	require.Len(t, attempts, 2)
	require.Equal(t, attempts[0].EventHash, attempts[1].EventHash)
	h.clock.Add(int64(jobLifetime))
	worked, err = h.server.DeliverOnce(t.Context())
	require.NoError(t, err)
	require.False(t, worked)
}

// TestMaintenanceCursorReachesExpiredRows proves bounded scans advance past
// live records and eventually free expired replay capacity.
func TestMaintenanceCursorReachesExpiredRows(t *testing.T) {
	h := newRelayHarness(t, nil)
	require.NoError(
		t, h.store.db.Update(func(tx *bolt.Tx) error {
			for i := range 300 {
				if err := putRecord(
					tx, dedupBucket, fmt.Sprintf("a-%04d",
						i),
					h.now().Add(time.Hour).Unix(),
				); err != nil {
					return err
				}
			}

			return putRecord(
				tx, dedupBucket, "z-expired",
				h.now().Add(-time.Second).Unix(),
			)
		}),
	)
	require.NoError(t, h.store.pruneExpired(t.Context(), h.now()))
	require.NoError(
		t, h.store.db.View(func(tx *bolt.Tx) error {
			require.NotNil(
				t,
				tx.Bucket(
					[]byte(dedupBucket)).Get([]byte(
					"z-expired",
				),
				),
			)

			return nil
		}),
	)
	require.NoError(t, h.store.pruneExpired(t.Context(), h.now()))
	require.NoError(
		t, h.store.db.View(func(tx *bolt.Tx) error {
			require.Nil(
				t,
				tx.Bucket(
					[]byte(dedupBucket)).Get([]byte(
					"z-expired",
				),
				),
			)

			return nil
		}),
	)
}

// TestPermanentProviderFailureRetires proves an explicit permanent outcome is
// not converted to a retry merely because it includes a diagnostic error.
func TestPermanentProviderFailureRetires(t *testing.T) {
	h := newRelayHarness(t, nil)
	sub := h.subscribe("wallet-route", false)
	require.NoError(
		t,
		h.store.enqueue(
			t.Context(), sub.WakeCapability, ScopeWake, "permanent",
			h.now(),
		),
	)
	calls := 0
	h.provider.send = func(context.Context, Delivery) (ProviderResult,
		error) {

		calls++

		return ProviderResult{Outcome: OutcomeRejected}, errors.New(
			"permanent provider " +
				"rejection")
	}
	worked, err := h.server.DeliverOnce(t.Context())
	require.NoError(t, err)
	require.True(t, worked)
	h.clock.Add(int64(time.Minute))
	worked, err = h.server.DeliverOnce(t.Context())
	require.NoError(t, err)
	require.False(t, worked)
	require.Equal(t, 1, calls)
}

// TestOlderInvalidationRetiresCurrentJob proves a current-token rejection with
// an old APNs timestamp neither retries a known rejection nor disables the
// installation registered after that timestamp. A replaced revision still
// retries through TestDelayedInvalidTokenResponse.
func TestOlderInvalidationRetiresCurrentJob(t *testing.T) {
	h := newRelayHarness(t, nil)
	sub := h.subscribe("wallet-route", false)
	require.NoError(
		t,
		h.store.enqueue(
			t.Context(), sub.WakeCapability, ScopeWake,
			"invalid-current", h.now(),
		),
	)
	calls := 0
	h.provider.send = func(context.Context, Delivery) (ProviderResult,
		error) {

		calls++

		return ProviderResult{
			Outcome:   OutcomeInvalidToken,
			InvalidAt: h.now().Add(-time.Minute),
		}, nil
	}
	worked, err := h.server.DeliverOnce(t.Context())
	require.NoError(t, err)
	require.True(t, worked)
	h.clock.Add(int64(time.Minute))
	worked, err = h.server.DeliverOnce(t.Context())
	require.NoError(t, err)
	require.False(t, worked)
	require.Equal(t, 1, calls)
	owner, err := h.store.loadInstallation(t.Context(), h.ownerID)
	require.NoError(t, err)
	require.False(t, owner.Disabled)
}
