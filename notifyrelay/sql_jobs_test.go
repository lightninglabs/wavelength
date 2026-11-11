package notifyrelay

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/lightninglabs/wavelength/notifyrelay/relaydb/sqlc"
	"github.com/stretchr/testify/require"
)

// sqlSubscription creates an app-owned route through the verified management
// transaction. Counter lookup is sequential test setup, not production auth.
func (h *sqlAuthHarness) sqlSubscription(t *testing.T, route string) string {
	t.Helper()
	owner, err := h.store.loadInstallation(t.Context(), "device")
	require.NoError(t, err)
	result, err := h.store.applyManagement(
		t.Context(), h.challenge(t), owner.Counter+1,
		ManagementPayload{
			Action:       "subscribe",
			RouteID:      route,
			EnableAlerts: true,
		},
		h.now,
	)
	require.NoError(t, err)

	return result.SubscriptionID
}

// sqlEnqueue uses the same transaction-owned boundary as source ingestion.
func (h *sqlAuthHarness) sqlEnqueue(t *testing.T, id string, scope Scope,
	event string) {

	t.Helper()
	require.NoError(
		t,
		h.database.Write(
			t.Context(),
			func(q *sqlc.Queries) error {
				sub, err := q.GetSubscription(t.Context(), id)
				if err != nil {
					return err
				}

				return enqueueSQLJobTx(
					t.Context(), q, sub, scope, event,
					h.now,
				)
			},
		),
	)
}

// TestSQLJobGenerationsAndReplicaLease prevents parallel route sends on one
// installation and preserves N+1 through old completion and quiet periods.
func TestSQLJobGenerationsAndReplicaLease(t *testing.T) {
	h := newSQLAuthHarness(t)
	first := h.sqlSubscription(t, "first")
	second := h.sqlSubscription(t, "second")
	h.sqlEnqueue(t, first, ScopeWake, "one")
	h.sqlEnqueue(t, second, ScopeWake, "two")
	peerDB := h.open()
	defer func() {
		require.NoError(t, peerDB.Close())
	}()
	peer, err := NewSQLStore(t.Context(), peerDB)
	require.NoError(t, err)
	deliveries := make(chan *Delivery, 2)
	failures := make(chan error, 2)
	var workers sync.WaitGroup
	for _, store := range []*SQLStore{h.store, peer} {
		workers.Go(func() {
			sent, err := store.claim(t.Context(), h.now)
			deliveries <- sent
			failures <- err
		})
	}
	workers.Wait()
	close(deliveries)
	close(failures)
	for err := range failures {
		require.NoError(t, err)
	}
	var sent *Delivery
	count := 0
	for value := range deliveries {
		if value != nil {
			count++
			sent = value
		}
	}
	require.Equal(t, 1, count, "different routes share the device lease")
	h.sqlEnqueue(t, sent.job.SubscriptionID, ScopeWake, "new-generation")
	require.NoError(
		t,
		h.store.complete(
			t.Context(), *sent, ProviderResult{
				Outcome: OutcomeAccepted,
			},
			h.now,
		),
	)
	row, err := h.database.Queries.GetJob(t.Context(), sent.job.ID)
	require.NoError(t, err)
	require.EqualValues(t, 2, row.Generation)
	require.EqualValues(t, 1, row.DoneGeneration)
	require.Equal(t, secretHash("new-generation"), row.EventHash)
	blocked, err := peer.claim(t.Context(), h.now)
	require.NoError(t, err)
	require.Nil(
		t, blocked,
		"another route must respect the installation quiet period",
	)
	next, err := peer.claim(t.Context(), h.now.Add(wakeQuietPeriod))
	require.NoError(t, err)
	require.NotNil(t, next)
	require.NoError(
		t,
		h.store.complete(
			t.Context(), *sent, ProviderResult{
				Outcome: OutcomeInvalidToken,
			},
			h.now.Add(wakeQuietPeriod),
		),
	)
	current, err := h.database.Queries.GetInstallation(
		t.Context(),
		"device",
	)
	require.NoError(t, err)
	require.False(t, current.Disabled)
	require.Equal(t, next.job.ClaimToken, current.DeliveryClaimToken)
	require.NoError(
		t,
		peer.complete(
			t.Context(), *next, ProviderResult{
				Outcome: OutcomeAccepted,
			},
			h.now.Add(wakeQuietPeriod),
		),
	)
}

// TestSQLJobLeaseTakeover fences a result from a lost worker after another
// database pool takes over its expired claim.
func TestSQLJobLeaseTakeover(t *testing.T) {
	h := newSQLAuthHarness(t)
	id := h.sqlSubscription(t, "wallet")
	h.sqlEnqueue(t, id, ScopeWake, "event")
	lost, err := h.store.claim(t.Context(), h.now)
	require.NoError(t, err)
	require.NotNil(t, lost)
	peerDB := h.open()
	defer func() {
		require.NoError(t, peerDB.Close())
	}()
	peer, err := NewSQLStore(t.Context(), peerDB)
	require.NoError(t, err)
	blocked, err := peer.claim(t.Context(), h.now)
	require.NoError(t, err)
	require.Nil(t, blocked)
	taken, err := peer.claim(
		t.Context(), h.now.Add(claimLifetime+time.Second),
	)
	require.NoError(t, err)
	require.NotNil(t, taken)
	require.NotEqual(t, lost.job.ClaimToken, taken.job.ClaimToken)
	require.NoError(
		t,
		h.store.complete(
			t.Context(), *lost, ProviderResult{
				Outcome: OutcomeAccepted,
			},
			h.now.Add(time.Minute),
		),
	)
	retained, err := h.database.Queries.GetJob(t.Context(), taken.job.ID)
	require.NoError(t, err)
	require.Equal(t, taken.job.ClaimToken, retained.ClaimToken)
	require.NoError(
		t,
		peer.complete(
			t.Context(), *taken, ProviderResult{
				Outcome: OutcomeAccepted,
			},
			h.now.Add(time.Minute),
		),
	)
	count, err := h.database.Queries.CountPendingJobs(t.Context())
	require.NoError(t, err)
	require.Zero(t, count)
}

// TestSQLInvalidTokenRecovery keeps bounded work for a disabled destination,
// without letting an old token response disable its signed replacement.
func TestSQLInvalidTokenRecovery(t *testing.T) {
	h := newSQLAuthHarness(t)
	id := h.sqlSubscription(t, "wallet")
	h.sqlEnqueue(t, id, ScopeWake, "first")
	old, err := h.store.claim(t.Context(), h.now)
	require.NoError(t, err)
	require.NotNil(t, old)
	_, err = h.store.applyManagement(
		t.Context(), h.challenge(t), 2, ManagementPayload{
			Action:      "rotate_token",
			DeviceToken: "replacement",
		},
		h.now,
	)
	require.NoError(t, err)
	require.NoError(
		t,
		h.store.complete(
			t.Context(), *old, ProviderResult{
				Outcome: OutcomeInvalidToken,
			},
			h.now,
		),
	)
	owner, err := h.store.loadInstallation(t.Context(), "device")
	require.NoError(t, err)
	require.False(t, owner.Disabled)
	h.now = h.now.Add(10 * time.Second)
	next, err := h.store.claim(t.Context(), h.now)
	require.NoError(t, err)
	require.NotNil(t, next)
	require.Equal(t, "replacement", next.DeviceToken)
	require.NoError(
		t,
		h.store.complete(
			t.Context(), *next, ProviderResult{
				Outcome: OutcomeInvalidToken,
			},
			h.now,
		),
	)
	h.sqlEnqueue(t, id, ScopeWake, "while-disabled")
	blocked, err := h.store.claim(t.Context(), h.now)
	require.NoError(t, err)
	require.Nil(t, blocked)
	_, err = h.store.applyManagement(
		t.Context(), h.challenge(t), 3, ManagementPayload{
			Action:      "rotate_token",
			DeviceToken: "recovered",
		},
		h.now,
	)
	require.NoError(t, err)
	recovered, err := h.store.claim(t.Context(), h.now)
	require.NoError(t, err)
	require.NotNil(t, recovered)
	require.Equal(t, "recovered", recovered.DeviceToken)
	require.EqualValues(t, 2, recovered.job.Generation)
}

// TestSQLPaymentReplayAndRollback preserves correlator dedup across a
// presentation change and rolls back local admission on a failed source commit.
func TestSQLPaymentReplayAndRollback(t *testing.T) {
	h := newSQLAuthHarness(t)
	id := h.sqlSubscription(t, "wallet")
	h.sqlEnqueue(t, id, ScopeAlert, "invoice-one")
	h.sqlEnqueue(t, id, ScopeAlert, "invoice-one")
	old, err := h.store.claim(t.Context(), h.now)
	require.NoError(t, err)
	require.NotNil(t, old)
	require.EqualValues(t, 1, old.job.Generation)
	_, err = h.store.applyManagement(
		t.Context(), h.challenge(t), 2, ManagementPayload{
			Action:       "subscribe",
			RouteID:      "wallet",
			EnableAlerts: true,
			Presentation: PresentationReceiptHint,
		},
		h.now,
	)
	require.NoError(t, err)
	h.sqlEnqueue(t, id, ScopeAlert, "invoice-one")
	count, err := h.database.Queries.CountPendingJobs(t.Context())
	require.NoError(t, err)
	require.Zero(
		t, count, "presentation change cannot replay the same payment",
	)
	h.sqlEnqueue(t, id, ScopeAlert, "invoice-two")
	require.NoError(
		t,
		h.store.complete(
			t.Context(), *old, ProviderResult{
				Outcome: OutcomeAccepted,
			},
			h.now,
		),
	)
	current, err := h.store.claim(t.Context(), h.now)
	require.NoError(t, err)
	require.Nil(
		t, current,
		"the old accepted send still consumes the quiet period",
	)
	current, err = h.store.claim(t.Context(), h.now.Add(alertQuietPeriod))
	require.NoError(t, err)
	require.NotNil(t, current)
	require.Equal(t, PresentationReceiptHint, current.Presentation)
	require.Equal(t, secretHash("invoice-two"), current.EventHash)
	before, err := h.database.Queries.CountDedup(t.Context())
	require.NoError(t, err)
	injected := errors.New("source checkpoint failed")
	err = h.database.Write(t.Context(), func(q *sqlc.Queries) error {
		sub, err := q.GetSubscription(t.Context(), id)
		if err != nil {
			return err
		}
		if err := enqueueSQLJobTx(
			t.Context(), q, sub, ScopeAlert, "uncommitted", h.now,
		); err != nil {
			return err
		}

		return injected
	})
	require.ErrorIs(t, err, injected)
	after, err := h.database.Queries.CountDedup(t.Context())
	require.NoError(t, err)
	require.Equal(t, before, after)
}

// TestSQLDedupQuotaIsPerInstallation prevents one device from consuming the
// shared replay budget while another device still has admission capacity.
func TestSQLDedupQuotaIsPerInstallation(t *testing.T) {
	h := newSQLAuthHarness(t)
	id := h.sqlSubscription(t, "wallet")
	ctx := t.Context()
	params := sqlc.PutDedupParams{
		SubscriptionID: id, InstallationID: "device",
		Scope: string(ScopeAlert), ExpiresAt: h.now.
			Add(dedupLifetime).
			Unix(),
	}
	require.NoError(
		t, h.database.Write(ctx, func(q *sqlc.Queries) error {
			for i := range maxDedupPerInstallation {
				params.EventHash = strconv.Itoa(i)
				if err := q.PutDedup(ctx, params); err != nil {
					return err
				}
			}

			return nil
		}),
	)
	err := h.database.Write(ctx, func(q *sqlc.Queries) error {
		sub, err := q.GetSubscription(ctx, id)
		if err != nil {
			return err
		}

		return enqueueSQLJobTx(ctx, q, sub, ScopeAlert, "full", h.now)
	})
	require.NoError(t, err)
	pending, err := h.database.Queries.GetJob(ctx, "alert/"+id)
	require.NoError(t, err)
	require.Equal(t, secretHash("full"), pending.EventHash)
	count, err := h.database.Queries.CountInstallationDedup(ctx, "device")
	require.NoError(t, err)
	require.EqualValues(t, maxDedupPerInstallation, count)
	sent, err := h.store.claim(ctx, h.now)
	require.NoError(t, err)
	require.Nil(t, sent, "only the quota-exhausted device waits")
	challenge, err := h.store.NewChallenge(
		ctx, ChallengeRequest{
			AppID:     "app",
			Operation: OperationEnroll,
		},
		h.now,
	)
	require.NoError(t, err)
	require.NoError(
		t,
		h.store.enroll(
			ctx, challenge, installation{
				ID:            "second",
				Policy:        AppConfig{ID: "app"},
				PublicKey:     []byte{2},
				DeviceToken:   "second",
				TokenRevision: 1,
			},
			h.now,
		),
	)
	challenge, err = h.store.NewChallenge(
		ctx, ChallengeRequest{
			AppID:          "app",
			Operation:      OperationManage,
			InstallationID: "second",
		},
		h.now,
	)
	require.NoError(t, err)
	result, err := h.store.applyManagement(
		ctx, challenge, 1, ManagementPayload{
			Action:       "subscribe",
			RouteID:      "wallet",
			EnableAlerts: true,
		},
		h.now,
	)
	require.NoError(t, err)
	h.sqlEnqueue(t, result.SubscriptionID, ScopeAlert, "second-device")
}

// TestSQLWakeReplayUsesCheckpointOnly proves ordinary wake coalescing does not
// consume the long-lived payment replay budget.
func TestSQLWakeReplayUsesCheckpointOnly(t *testing.T) {
	h := newSQLAuthHarness(t)
	id := h.sqlSubscription(t, "wallet")
	h.sqlEnqueue(t, id, ScopeWake, "generation-one")
	h.sqlEnqueue(t, id, ScopeWake, "generation-two")
	count, err := h.database.Queries.CountDedup(context.Background())
	require.NoError(t, err)
	require.Zero(t, count)
}

// TestSQLPruneExpiredObligations removes bounded expired work while keeping
// still-live payment replay evidence and subscriptions until their own expiry.
func TestSQLPruneExpiredObligations(t *testing.T) {
	h := newSQLAuthHarness(t)
	id := h.sqlSubscription(t, "wallet")
	h.sqlEnqueue(t, id, ScopeWake, "wake")
	h.sqlEnqueue(t, id, ScopeAlert, "payment")
	require.NoError(
		t,
		h.store.pruneExpired(
			t.Context(), h.now.Add(jobLifetime+time.Second),
		),
	)
	jobs, err := h.database.Queries.CountPendingJobs(t.Context())
	require.NoError(t, err)
	require.Zero(t, jobs)
	dedup, err := h.database.Queries.CountDedup(t.Context())
	require.NoError(t, err)
	require.EqualValues(t, 1, dedup)
	require.NoError(
		t,
		h.store.pruneExpired(
			t.Context(),
			h.now.Add(subscriptionLifetime+time.Second),
		),
	)
	subscriptions, err := h.database.Queries.CountInstallationSubscriptions(
		t.Context(),
		"device",
	)
	require.NoError(t, err)
	require.Zero(t, subscriptions)
	dedup, err = h.database.Queries.CountDedup(t.Context())
	require.NoError(t, err)
	require.Zero(t, dedup)
}

// TestSQLPrunedSendStillConsumesQuietPeriod exercises expiry while provider I/O
// owns the installation lease and a different live route is already pending.
func TestSQLPrunedSendStillConsumesQuietPeriod(t *testing.T) {
	h := newSQLAuthHarness(t)
	first := h.sqlSubscription(t, "old")
	second := h.sqlSubscription(t, "new")
	h.sqlEnqueue(t, first, ScopeWake, "old")
	h.now = h.now.Add(jobLifetime - time.Second)
	sent, err := h.store.claim(t.Context(), h.now)
	require.NoError(t, err)
	require.NotNil(t, sent)
	h.now = h.now.Add(2 * time.Second)
	deleted, err := h.database.Queries.PruneExpiredJobs(
		t.Context(), sqlc.PruneExpiredJobsParams{
			ExpiresAt: h.now.UnixNano(),
			Limit:     32,
		},
	)
	require.NoError(t, err)
	require.EqualValues(t, 1, deleted)
	h.sqlEnqueue(t, second, ScopeWake, "new")
	require.NoError(
		t,
		h.store.complete(
			t.Context(), *sent, ProviderResult{
				Outcome: OutcomeAccepted,
			},
			h.now,
		),
	)
	ready, err := h.store.claim(t.Context(), h.now)
	require.NoError(t, err)
	require.Nil(t, ready)
	ready, err = h.store.claim(t.Context(), h.now.Add(wakeQuietPeriod))
	require.NoError(t, err)
	require.NotNil(t, ready)
	require.Equal(t, second, ready.job.SubscriptionID)
}

// TestSQLRetryBudgetStartsWithSuccessor keeps historical successful sends from
// inflating the first transient retry delay for a newer coalesced generation.
func TestSQLRetryBudgetStartsWithSuccessor(t *testing.T) {
	h := newSQLAuthHarness(t)
	id := h.sqlSubscription(t, "wallet")
	h.sqlEnqueue(t, id, ScopeWake, "one")
	sent, err := h.store.claim(t.Context(), h.now)
	require.NoError(t, err)
	require.NotNil(t, sent)
	h.sqlEnqueue(t, id, ScopeWake, "two")
	require.NoError(
		t,
		h.store.complete(
			t.Context(), *sent, ProviderResult{
				Outcome: OutcomeAccepted,
			},
			h.now,
		),
	)
	next, err := h.store.claim(t.Context(), h.now.Add(wakeQuietPeriod))
	require.NoError(t, err)
	require.NotNil(t, next)
	require.EqualValues(t, 1, next.job.Attempts)
}
