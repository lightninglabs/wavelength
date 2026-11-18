package notifyrelay

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	pb "github.com/lightninglabs/wavelength/mailbox/pb"
	"github.com/lightninglabs/wavelength/notification"
	"github.com/lightninglabs/wavelength/notifyrelay/relaydb/sqlc"
	"github.com/stretchr/testify/require"
)

// TestFeedRevocationKeepsOnlyReplacementRoundWake enrolls two keys for one
// device token, then disposes of the old key. A producer removal must cancel
// the old queued wake, and future rounds must wake just the replacement.
func TestFeedRevocationKeepsOnlyReplacementRoundWake(t *testing.T) {
	ctx := t.Context()
	cfg := testSourceConfig(t, "operator")
	cfg.EventClasses = notification.FeedClassWake |
		notification.FeedClassRound
	h := sourceHarness(t, cfg)
	source := h.store.sources[cfg.ID]

	old := newRelayHarness(t, h.store)
	newDevice := newRelayHarness(t, h.store)
	oldSub := old.subscribe("wallet", false)
	newSub := newDevice.subscribe("wallet", false)
	require.NotEqual(t, oldSub.SubscriptionID, newSub.SubscriptionID)
	oldToken := bytes.Repeat([]byte{1}, notification.FeedTokenSize)
	newToken := bytes.Repeat([]byte{2}, notification.FeedTokenSize)
	bind := func(sub ManagementResult, token []byte) ManagementPayload {
		return ManagementPayload{
			Action:         "bind_feed",
			SubscriptionID: sub.SubscriptionID,
			FeedBinding: &FeedBindingRequest{
				SourceID: cfg.ID,
				FeedToken: base64.RawURLEncoding.EncodeToString(
					token,
				),
				EventClasses:  cfg.EventClasses,
				ExpiresAtUnix: sub.ExpiresAt,
			},
		}
	}
	oldBind := bind(oldSub, oldToken)
	require.Equal(t, http.StatusOK, old.manage(oldBind).Code)
	require.Equal(
		t, http.StatusOK, newDevice.manage(bind(newSub, newToken)).Code,
	)
	raw, err := json.Marshal(oldBind)
	require.NoError(t, err)
	stale := old.assertion(
		old.challenge(OperationManage), raw, old.counter.Add(1),
		old.app.AppleAppID,
	)
	old.key = nil

	schedule := func(open time.Time) {
		t.Helper()
		deadline := open.Add(50 * time.Second)
		require.NoError(
			t,
			h.store.scheduleRoundBindings(
				ctx, cfg.ID, deadline.Unix(), open, deadline,
				h.now,
			),
		)
	}
	open := h.now.Add(time.Minute)
	schedule(open)
	count, err := h.database.Queries.CountPendingJobs(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 2, count)

	envelope, err := notification.NewFeedRevocationEnvelope(
		source.public.SerializeCompressed(), source.mailboxID,
		oldSub.SubscriptionID, oldToken, 2,
	)
	require.NoError(t, err)
	envelope.EventSeq = 1
	event := envelopeEvent(t, envelope)
	require.NoError(t, notification.ValidateFeedEvent(event))
	require.Less(
		t, event.ExpiresAtUnix, h.now.Unix(),
		"legacy readers must consume this without creating a wake",
	)

	ingress := &feedIngress{store: h.store, source: source}
	envelope.Sender = "wrong-producer"
	_, err = ingress.validateBatch(feedBatch(envelope), 0)
	require.ErrorIs(t, err, ErrFeedSource)
	envelope.Sender = cfg.PublicKey

	ingress.mailbox = &feedTransport{
		pull: func(context.Context, *pb.PullRequest) (*pb.PullResponse,
			error) {

			return feedBatch(envelope), nil
		},
		ack: func(ctx context.Context, r *pb.AckUpToRequest) (
			*pb.AckUpToResponse, error) {

			require.EqualValues(t, 2, r.Cursor)
			_, err := h.database.Queries.GetJob(
				ctx, "schedule/"+oldSub.SubscriptionID,
			)
			require.ErrorIs(
				t, err, sql.ErrNoRows,
				"retire the wake before ACK",
			)

			return &pb.AckUpToResponse{
				Status: &pb.Status{
					Ok: true,
				},
			}, nil
		},
	}
	worked, err := ingress.step(ctx)
	require.NoError(t, err)
	require.True(t, worked)
	response := old.post(
		"/v1/installations/"+old.ownerID+"/manage", stale, "",
	)
	require.Equal(t, http.StatusUnauthorized, response.Code)
	require.Contains(t, response.Body.String(), `"unauthorized"`)

	// Stale traffic can still be in transit when the producer commits
	// removal. Even a fresh token cannot resurrect this source binding.
	admitRoundWake(t, h, ingress, oldSub.SubscriptionID, 3, open)
	count, err = h.database.Queries.CountPendingJobs(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 1, count)

	var delivered []Delivery
	newDevice.provider.send = func(_ context.Context, d Delivery) (
		ProviderResult, error) {

		delivered = append(delivered, d)

		return ProviderResult{Outcome: OutcomeAccepted}, nil
	}
	for _, at := range []time.Time{open, open.Add(5 * time.Minute)} {
		schedule(at)
		newDevice.clock.Store(at.UnixNano())
		worked, err := newDevice.server.DeliverOnce(ctx)
		require.NoError(t, err)
		require.True(t, worked)
		worked, err = newDevice.server.DeliverOnce(ctx)
		require.NoError(t, err)
		require.False(t, worked)
	}
	require.Len(t, delivered, 2)
	for _, wake := range delivered {
		require.Equal(t, ScopeSchedule, wake.job.Scope)
		require.Equal(t, "schedule/"+newSub.SubscriptionID, wake.job.ID)
		require.Equal(t, newDevice.ownerID, wake.installationID)
	}
}

// TestFeedRevocationBeforeBind fences a delayed bind from only the revoked
// source and retains a wake authorized by an independent producer.
func TestFeedRevocationBeforeBind(t *testing.T) {
	source := testSourceConfig(t, "first")
	other := testSourceConfig(t, "second")
	h := sourceHarness(t, source, other)
	ctx := t.Context()
	id := h.sqlSubscription(t, "wallet")
	token := bytes.Repeat([]byte{8}, notification.FeedTokenSize)
	binding := FeedBindingRequest{
		SourceID:      other.ID,
		FeedToken:     base64.RawURLEncoding.EncodeToString(token),
		EventClasses:  notification.FeedClassWake,
		ExpiresAtUnix: h.now.Add(time.Hour).Unix(),
	}
	require.NoError(t, h.bindTestFeed(t, id, binding))
	h.sqlEnqueue(t, id, ScopeWake, "")
	f := &feedIngress{store: h.store, source: h.store.sources[source.ID]}
	require.NoError(
		t, h.database.Write(ctx, func(q *sqlc.Queries) error {
			return f.revokeBinding(ctx, q, &pb.WakeFeedEvent{
				GrantId: id, FeedToken: token,
			}, h.now)
		}),
	)
	binding.SourceID = source.ID
	require.ErrorIs(t, h.bindTestFeed(t, id, binding), ErrNotFound)
	job, err := h.database.Queries.GetJob(ctx, "wake/"+id)
	require.NoError(t, err)
	require.Equal(t, id, job.SubscriptionID)
}
