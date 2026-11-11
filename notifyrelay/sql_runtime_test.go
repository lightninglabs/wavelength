package notifyrelay

import (
	"bytes"
	"context"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/lightninglabs/wavelength/mailbox/pb"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/anypb"
)

// TestSQLRuntimeOwnsSourceAndDelivery composes real HTTP signed device setup,
// SQL persistence, TLS mailbox ingress and a controlled provider call. Cancel
// joins the subscriber and provider without erasing ambiguous in-flight work.
func TestSQLRuntimeOwnsSourceAndDelivery(t *testing.T) {
	h := newRelayHarness(t, nil)
	database := sqlTestOpener(t)()
	defer func() {
		require.NoError(t, database.Close())
	}()
	store, err := NewSQLStore(
		t.Context(), database, testSourceConfig(t, "producer"),
	)
	require.NoError(t, err)
	deliveries := make(chan Delivery, 1)
	var active atomic.Int32
	provider := &testProvider{send: func(ctx context.Context, d Delivery) (
		ProviderResult, error) {

		active.Add(1)
		defer active.Add(-1)
		deliveries <- d
		<-ctx.Done()

		return ProviderResult{
			Outcome: OutcomeRetry,
		}, ctx.Err()
	}}
	server, err := NewServer(
		Config{
			Applications: []AppConfig{h.app},
			Authenticator: &AppAttestAuthenticator{
				verifier: &testEnrollmentVerifier{
					public: &h.key.PublicKey,
				},
			},
		},
		store,
		provider,
	)
	require.NoError(t, err)
	h.server = server
	h.enroll()
	result := h.subscribe("wallet-route", false)
	require.Len(t, result.FeedOffers, 1)
	require.Equal(t, result.SubscriptionID, result.FeedOffers[0].GrantID)
	// No app bind has returned yet. The authenticated source still records
	// this first wake against the enrolled random route.
	event := &pb.WakeFeedEvent{GrantId: result.SubscriptionID,
		FeedToken:  bytes.Repeat([]byte{8}, 32),
		Generation: 1, EventClasses: 1,
		ExpiresAtUnix: time.Now().Add(time.Hour).Unix()}
	body, err := anypb.New(event)
	require.NoError(t, err)
	source := store.sources["producer"]
	envelope := &pb.Envelope{ProtocolVersion: pb.MailboxProtocolVersionV1,
		EventSeq: 1, Sender: source.config.PublicKey,
		Recipient: source.mailboxID, Body: body,
		Rpc: &pb.RpcMeta{
			Kind:    pb.RpcMeta_KIND_EVENT,
			Service: feedServiceName,
			Method:  "Wake",
		}}
	producer := &feedRPCFixture{
		source:       source,
		event:        envelope,
		emptyStarted: make(chan struct{}),
	}
	startFeedRPCFixture(t, producer)
	for _, path := range []string{
		"/v1/wakes",
		"/v1/alerts",
		"/v1/subscriptions/verify",
	} {
		response := publisherPost(
			t, server, path, map[string]string{
				"event_id": "ignored",
			},
		)
		require.Equal(
			t, http.StatusNotFound, response.Code,
			"retired producer API",
		)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- server.Run(ctx)
	}()
	select {
	case sent := <-deliveries:
		require.Equal(t, "wallet-route", sent.RouteID)
		require.Equal(t, ScopeWake, sent.Scope)
		require.Equal(t, "aabbccdd", sent.DeviceToken)
		require.Equal(t, h.app.Topic, sent.Topic)

	case <-time.After(10 * time.Second):
		t.Fatal("source job was not delivered")
	}
	select {
	case <-producer.emptyStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("source did not resume Pull")
	}
	state, err := database.Queries.GetSource(t.Context(), "producer")
	require.NoError(t, err)
	require.EqualValues(t, 2, state.Cursor)
	require.False(t, state.AckPending)
	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)

	case <-time.After(5 * time.Second):
		t.Fatal("Run did not join its workers")
	}
	require.Zero(t, active.Load())
	require.False(t, server.running.Load())
	job, err := database.Queries.GetJob(
		t.Context(), "wake/"+result.SubscriptionID,
	)
	require.NoError(t, err)
	require.Zero(
		t, job.DoneGeneration,
		"cancellation retains ambiguous delivery",
	)
	require.NotEmpty(t, job.ClaimToken)
	metrics := httptest.NewRecorder()
	server.metrics(
		metrics, httptest.NewRequest(http.MethodGet, "/metrics", nil),
	)
	require.Contains(
		t, metrics.Body.String(),
		"notification_relay_pending_jobs 1",
	)
	require.NotContains(t, metrics.Body.String(), result.SubscriptionID)
	require.NotContains(
		t, metrics.Body.String(), hex.EncodeToString(event.FeedToken),
	)
}
