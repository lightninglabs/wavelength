package notifyrelay

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"sync"
	"testing"
	"time"

	pb "github.com/lightninglabs/wavelength/mailbox/pb"
	"github.com/lightninglabs/wavelength/notifyrelay/relaydb/sqlc"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// fillPendingJobBudget creates storage fixtures for verified devices, with at
// most sixteen routes and two jobs per device. It exercises real global queue
// exhaustion without inventing an impossible thousands-of-routes installation.
func fillPendingJobBudget(t *testing.T, h *sqlAuthHarness, needed int) {
	t.Helper()
	ctx := t.Context()
	err := h.database.Write(ctx, func(q *sqlc.Queries) error {
		remaining := needed
		sub := sqlc.PutSubscriptionParams{
			AlertsEnabled: true, Presentation: "generic",
			ExpiresAt: h.now.Add(subscriptionLifetime).Unix(),
			Revision:  1,
		}
		job := sqlc.CoalesceJobParams{
			EventHash: "fixture", NextAt: h.now.UnixNano(),
			ExpiresAt: h.now.Add(jobLifetime).UnixNano(),
		}
		for device := 0; remaining > 0; device++ {
			owner := fmt.Sprintf("capacity-device-%d", device)
			value := installation{ID: owner, Policy: AppConfig{
				ID: "app",
			},
				PublicKey:     []byte("verified fixture"),
				DeviceToken:   "fixture",
				TokenRevision: 1}
			if err := saveSQLInstallation(
				ctx, q, value,
			); err != nil {
				return err
			}
			sub.InstallationID = owner
			for route := range maxSubscriptions {
				seed := []byte(
					fmt.Sprintf("%024d",
						device*maxSubscriptions+route),
				)
				id := base64.RawURLEncoding.EncodeToString(seed)
				sub.ID, sub.RouteID = id, fmt.Sprint(route)
				if _, err := q.PutSubscription(
					ctx, sub,
				); err != nil {
					return err
				}
				for _, scope := range []Scope{
					ScopeWake,
					ScopeAlert,
				} {
					if remaining == 0 {
						break
					}
					job.ID, job.SubscriptionID = string(
						scope,
					)+"/"+id, id
					job.Scope = string(scope)
					if err := q.CoalesceJob(
						ctx, job,
					); err != nil {
						return err
					}
					remaining--
				}
				if remaining == 0 {
					break
				}
			}
		}

		return nil
	})
	require.NoError(t, err)
}

// replayQuota fills accepted replay evidence without consuming pending jobs.
func replayQuota(t *testing.T, h *sqlAuthHarness, subscription string,
	expiry time.Time) {

	t.Helper()
	params := sqlc.PutDedupParams{SubscriptionID: subscription,
		InstallationID: "device",
		Scope:          "alert", ExpiresAt: expiry.Unix()}
	err := h.database.Write(t.Context(), func(q *sqlc.Queries) error {
		for i := range maxDedupPerInstallation {
			params.EventHash = fmt.Sprint(i)
			if err := q.PutDedup(t.Context(), params); err != nil {
				return err
			}
		}

		return nil
	})
	require.NoError(t, err)
}

// secondReplayRoute uses the verified enrollment/management storage boundary
// to create a second device's independently owned route.
func secondReplayRoute(t *testing.T, h *sqlAuthHarness) string {
	t.Helper()
	ctx := t.Context()
	challenge, err := h.store.NewChallenge(ctx, ChallengeRequest{
		AppID: "app", Operation: OperationEnroll,
	}, h.now)
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
	challenge, err = h.store.NewChallenge(ctx, ChallengeRequest{
		AppID: "app", Operation: OperationManage,
		InstallationID: "second",
	}, h.now)
	require.NoError(t, err)
	result, err := h.store.applyManagement(
		ctx, challenge, 1, ManagementPayload{
			Action:       "subscribe",
			RouteID:      "second-wallet",
			EnableAlerts: true,
		},
		h.now,
	)
	require.NoError(t, err)

	return result.SubscriptionID
}

// TestFeedIngressReplayQuotaIsolation proves one device cannot hold the
// aggregate source prefix. It retains a bounded candidate while another
// installation records its job, source ACK and provider claim normally.
func TestFeedIngressReplayQuotaIsolation(t *testing.T) {
	h, f, first := ingressHarness(t)
	event := envelopeEvent(t, first)
	event.EventClasses = 2
	replaceFeedEvent(t, first, event)
	replayQuota(t, h, event.GrantId, h.now.Add(dedupLifetime))
	secondID := secondReplayRoute(t, h)
	secondEvent, ok := proto.Clone(event).(*pb.WakeFeedEvent)
	require.True(t, ok)
	secondEvent.GrantId = secondID
	secondEvent.FeedToken = bytes.Repeat([]byte{6}, 32)
	secondEvent.PaymentEventId = fmt.Sprintf("%064x", 2)
	second, ok := proto.Clone(first).(*pb.Envelope)
	require.True(t, ok)
	second.EventSeq = 2
	replaceFeedEvent(t, second, secondEvent)
	acked := false
	f.mailbox = &feedTransport{
		pull: func(context.Context, *pb.PullRequest) (*pb.PullResponse,
			error) {

			return &pb.PullResponse{
				Status: &pb.Status{
					Ok: true,
				},
				Envelopes: []*pb.Envelope{
					first,
					second,
				},
				NextCursor: 3,
			}, nil
		},
		ack: func(_ context.Context, r *pb.AckUpToRequest) (
			*pb.AckUpToResponse, error) {

			require.EqualValues(t, 3, r.Cursor)
			acked = true

			return &pb.AckUpToResponse{
				Status: &pb.Status{
					Ok: true,
				},
			}, nil
		},
	}
	worked, err := f.step(t.Context())
	require.NoError(t, err)
	require.True(t, worked)
	require.True(t, acked)
	count, err := h.database.Queries.CountPendingJobs(t.Context())
	require.NoError(t, err)
	require.EqualValues(t, 2, count)
	firstBudget, err := h.database.Queries.CountInstallationDedup(
		t.Context(),
		"device",
	)
	require.NoError(t, err)
	require.EqualValues(t, maxDedupPerInstallation, firstBudget)
	secondBudget, err := h.database.Queries.CountInstallationDedup(
		t.Context(),
		"second",
	)
	require.NoError(t, err)
	require.EqualValues(t, 1, secondBudget)
	sent, err := h.store.claim(t.Context(), time.Now())
	require.NoError(t, err)
	require.NotNil(t, sent)
	require.Equal(t, "second", sent.installationID)
	pending, err := h.database.Queries.GetJob(
		t.Context(), "alert/"+event.GrantId,
	)
	require.NoError(t, err)
	require.Empty(t, pending.ClaimToken, "over-budget payment is not sent")
	state, err := h.database.Queries.GetSource(t.Context(), "producer")
	require.NoError(t, err)
	require.EqualValues(t, 3, state.Cursor)
	require.False(t, state.AckPending)
}

// TestDeferredPaymentReplayAdmission reserves replay space before sending a
// deferred candidate once maintenance frees capacity within its expiry.
func TestDeferredPaymentReplayAdmission(t *testing.T) {
	h := newSQLAuthHarness(t)
	id := h.sqlSubscription(t, "wallet")
	replayQuota(t, h, id, h.now.Add(time.Minute))
	h.sqlEnqueue(t, id, ScopeAlert, "deferred-payment")
	sent, err := h.store.claim(t.Context(), h.now)
	require.NoError(t, err)
	require.Nil(t, sent)
	after := h.now.Add(2 * time.Minute)
	require.NoError(t, h.store.pruneExpired(t.Context(), after))
	peerDB := h.open()
	defer func() {
		require.NoError(t, peerDB.Close())
	}()
	peer, err := NewSQLStore(t.Context(), peerDB)
	require.NoError(t, err)
	claims := make(chan *Delivery, 2)
	failures := make(chan error, 2)
	var workers sync.WaitGroup
	for _, store := range []*SQLStore{h.store, peer} {
		workers.Go(func() {
			delivery, err := store.claim(t.Context(), after)
			claims <- delivery
			failures <- err
		})
	}
	workers.Wait()
	close(claims)
	close(failures)
	for err := range failures {
		require.NoError(t, err)
	}
	winners := 0
	for delivery := range claims {
		if delivery != nil {
			sent = delivery
			winners++
		}
	}
	require.Equal(
		t, 1, winners, "one replay reservation and provider owner",
	)
	require.NotNil(t, sent)
	expiry, err := h.database.Queries.GetDedup(
		t.Context(), sqlc.GetDedupParams{
			SubscriptionID: id,
			Scope:          "alert",
			EventHash: secretHash(
				"deferred-payment",
			),
		},
	)
	require.NoError(t, err)
	require.Equal(t, after.Add(dedupLifetime).Unix(), expiry)
	require.NoError(
		t,
		h.store.complete(
			t.Context(), *sent, ProviderResult{
				Outcome: OutcomeAccepted,
			},
			after,
		),
	)
	h.sqlEnqueue(t, id, ScopeAlert, "deferred-payment")
	count, err := h.database.Queries.CountPendingJobs(t.Context())
	require.NoError(t, err)
	require.Zero(t, count, "admitted identity remains replay protected")
}

// TestDeferredPageReportsProgress keeps a full replay-blocked page from adding
// one idle sleep per page before an ordinary wake can reach the provider.
func TestDeferredPageReportsProgress(t *testing.T) {
	h := newSQLAuthHarness(t)
	first := h.sqlSubscription(t, "first")
	second := secondReplayRoute(t, h)
	owners := []struct{ owner, first string }{
		{
			"device",
			first,
		}, {
			"second",
			second,
		},
	}
	for _, owner := range owners {
		err := h.database.Write(
			t.Context(), func(q *sqlc.Queries) error {
				for i := range maxDedupPerInstallation {
					params := sqlc.PutDedupParams{
						SubscriptionID: owner.first,
						InstallationID: owner.owner,
						Scope:          "alert",
						EventHash:      fmt.Sprint(i),
						ExpiresAt: h.now.Add(
							dedupLifetime,
						).Unix(),
					}
					if err := q.PutDedup(
						t.Context(), params,
					); err != nil {
						return err
					}
				}

				return nil
			},
		)
		require.NoError(t, err)
		for route := range maxSubscriptions {
			id := owner.first
			if route != 0 {
				value, err := h.store.loadInstallation(
					t.Context(), owner.owner,
				)
				require.NoError(t, err)
				challenge, err := h.store.NewChallenge(
					t.Context(), ChallengeRequest{
						AppID:          "app",
						Operation:      OperationManage,
						InstallationID: owner.owner,
					},
					h.now,
				)
				require.NoError(t, err)
				result, err := h.store.applyManagement(
					t.Context(), challenge, value.Counter+1,
					ManagementPayload{
						Action: "subscribe",
						RouteID: fmt.Sprint(
							route,
						),
						EnableAlerts: true,
					},
					h.now,
				)
				require.NoError(t, err)
				id = result.SubscriptionID
			}
			h.sqlEnqueue(t, id, ScopeAlert, "deferred")
		}
	}
	h.sqlEnqueue(t, first, ScopeWake, "wake")
	delivery, progressed, err := h.store.claimWithProgress(
		t.Context(), h.now,
	)
	require.NoError(t, err)
	require.Nil(t, delivery)
	require.True(t, progressed)
	delivery, _, err = h.store.claimWithProgress(t.Context(), h.now)
	require.NoError(t, err)
	require.NotNil(t, delivery)
	require.Equal(t, ScopeWake, delivery.Scope)
}
