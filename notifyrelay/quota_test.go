package notifyrelay

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	bolt "go.etcd.io/bbolt"
)

// TestInstallationDedupQuota isolates installations across routes and scopes,
// preserves replay at capacity and charges expired evidence until pruning.
func TestInstallationDedupQuota(t *testing.T) {
	h := newRelayHarness(t, nil)
	other := newRelayHarness(t, h.store)
	subs := []ManagementResult{
		h.subscribe("wallet-a", true),
		h.subscribe("wallet-b", true),
	}
	foreign := other.subscribe("wallet-c", false)
	for i := range maxDedupPerInstallation {
		sub := subs[i%len(subs)]
		capability := sub.WakeCapability
		path := "/v1/wakes"
		if i%4 >= 2 {
			capability = sub.AlertCapability
			path = "/v1/alerts"
		}
		request := PushRequest{EventID: fmt.Sprintf("event-%d", i)}
		require.Equal(
			t, http.StatusAccepted, h.post(
				path, request,
				capability,
			).Code,
		)
	}
	fresh := PushRequest{EventID: "fresh"}
	for _, sub := range subs {
		require.Equal(
			t, http.StatusTooManyRequests, h.post(
				"/v1/wakes", fresh, sub.WakeCapability,
			).Code,
		)
	}
	require.Equal(
		t, http.StatusAccepted, other.post(
			"/v1/wakes", fresh, foreign.WakeCapability,
		).Code,
	)

	// Rotation preserves subscription identity and the old event's replay
	// evidence. A replay at capacity must not advance the pending job.
	renewed := h.subscribe("wallet-a", true)
	require.Equal(t, subs[0].SubscriptionID, renewed.SubscriptionID)
	require.Equal(
		t, http.StatusAccepted, h.post(
			"/v1/wakes", PushRequest{EventID: "event-0"},
			renewed.WakeCapability,
		).Code,
	)
	require.NoError(
		t, h.store.db.View(func(tx *bolt.Tx) error {
			job, err := readRecord[relayJob](
				tx, jobsBucket,
				string(ScopeWake)+"/"+renewed.SubscriptionID,
			)
			require.NoError(t, err)
			require.EqualValues(
				t, maxDedupPerInstallation/4, job.Generation,
			)
			require.Equal(
				t, maxDedupPerInstallation,
				installationDedupCount(tx, h.ownerID),
			)

			return nil
		}),
	)

	// Expiry alone does not free retained storage. The existing bounded
	// maintenance cursor must actually remove records before admission.
	h.clock.Add(int64(dedupLifetime + time.Second))
	require.Equal(
		t, http.StatusTooManyRequests, h.post(
			"/v1/wakes", fresh, renewed.WakeCapability,
		).Code,
	)
	for range (maxDedupPerInstallation+1)/256 + 1 {
		require.NoError(t, h.store.pruneExpired(t.Context(), h.now()))
	}
	require.Equal(
		t, http.StatusAccepted, h.post(
			"/v1/wakes", fresh, renewed.WakeCapability,
		).Code,
	)
}

// TestRevocationReleasesDedupQuota proves deleting or expiring a route cannot
// leave uncharged replay records. Revoked grants fail and recreation gets a
// fresh identity; a deleted installation cannot resume admission.
func TestRevocationReleasesDedupQuota(t *testing.T) {
	for _, action := range []string{"revoke", "expire", "delete"} {
		t.Run(action, func(t *testing.T) {
			h := newRelayHarness(t, nil)
			sub := h.subscribe("wallet", false)
			request := PushRequest{EventID: "event"}
			if action == "expire" {
				elapsed := subscriptionLifetime - time.Hour
				h.clock.Add(int64(elapsed))
			}
			require.Equal(
				t, http.StatusAccepted, h.post(
					"/v1/wakes", request,
					sub.WakeCapability,
				).Code,
			)
			switch action {
			case "revoke":
				action := ManagementPayload{
					Action:         "revoke_subscription",
					SubscriptionID: sub.SubscriptionID,
				}
				require.Equal(
					t, http.StatusOK, h.manage(action).Code,
				)

			case "expire":
				elapsed := 2 * time.Hour
				h.clock.Add(int64(elapsed))
				require.NoError(
					t,
					h.store.pruneExpired(
						t.Context(), h.now(),
					),
				)

			case "delete":
				require.Equal(
					t, http.StatusOK,
					h.manage(ManagementPayload{
						Action: "delete_installation",
					}).Code,
				)
			}
			require.Equal(
				t, http.StatusUnauthorized, h.post(
					"/v1/wakes", request,
					sub.WakeCapability,
				).Code,
			)
			require.NoError(
				t, h.store.db.View(func(tx *bolt.Tx) error {
					expected := 0
					if action == "delete" {
						expected = 1
					}
					dedup := tx.Bucket([]byte(dedupBucket))
					require.Equal(
						t, expected, dedup.Stats().KeyN,
					)
					count := installationDedupCount(
						tx, h.ownerID,
					)
					require.Equal(t, expected, count)

					return nil
				}),
			)
			if action != "delete" {
				replacement := h.subscribe("wallet", false)
				require.NotEqual(
					t, sub.SubscriptionID,
					replacement.SubscriptionID,
				)
				require.Equal(
					t, http.StatusAccepted, h.post(
						"/v1/wakes", request,
						replacement.WakeCapability,
					).Code,
				)
			}
		})
	}
}
