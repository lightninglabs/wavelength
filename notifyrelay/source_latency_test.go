package notifyrelay

import (
	"bytes"
	"context"
	"log/slog"
	"testing"
	"time"

	pb "github.com/lightninglabs/wavelength/mailbox/pb"
	"github.com/lightninglabs/wavelength/notifyrelay/relaydb/sqlc"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/anypb"
)

// idleDeliveryStore observes empty delivery passes without adding a test-only
// production hook. The source worker is started explicitly and joined below.
type idleDeliveryStore struct {
	*SQLStore
	idle chan struct{}
}

// claimWithProgress exposes when the delivery loop has reached its wait point.
func (s *idleDeliveryStore) claimWithProgress(ctx context.Context,
	now time.Time) (*Delivery, bool, error) {

	delivery, progress, err := s.SQLStore.claimWithProgress(ctx, now)
	if err == nil && delivery == nil && !progress {
		select {
		case s.idle <- struct{}{}:
		default:
		}
	}

	return delivery, progress, err
}

// TestSourceCommitWakesDeliveryBeforeAck proves the post-commit signal reaches
// the actual delivery loop before the periodic tick or a stalled remote ACK.
func TestSourceCommitWakesDeliveryBeforeAck(t *testing.T) {
	h := newRelayHarness(t, nil)
	subscription := h.subscribe("latency-route", false)
	source := h.store.sources["producer"]
	body, err := anypb.New(&pb.WakeFeedEvent{
		GrantId:   subscription.SubscriptionID,
		FeedToken: bytes.Repeat([]byte{8}, 32), Generation: 1,
		EventClasses: 1, ExpiresAtUnix: h.now().Add(time.Hour).Unix(),
	})
	require.NoError(t, err)
	envelope := &pb.Envelope{
		ProtocolVersion: pb.MailboxProtocolVersionV1, EventSeq: 1,
		Sender: source.config.PublicKey, Recipient: source.mailboxID,
		Body: body, Rpc: &pb.RpcMeta{
			Kind: pb.RpcMeta_KIND_EVENT, Service: feedServiceName,
			Method: "Wake",
		},
	}
	available := make(chan struct{})
	ackStarted := make(chan struct{}, 1)
	providerStarted := make(chan struct{}, 1)
	producer := &feedRPCFixture{source: source, event: envelope}
	producer.pull = func(ctx context.Context, request *pb.PullRequest) (
		*pb.PullResponse, error) {

		if request.Cursor > envelope.EventSeq {
			<-ctx.Done()

			return nil, ctx.Err()
		}
		select {
		case <-available:
			return feedBatch(envelope), nil

		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	producer.ack = func(ctx context.Context, _ *pb.AckUpToRequest) (
		*pb.AckUpToResponse, error) {

		ackStarted <- struct{}{}
		<-ctx.Done()

		return nil, ctx.Err()
	}
	startFeedRPCFixture(t, producer)
	h.provider.send = func(ctx context.Context, _ Delivery) (ProviderResult,
		error) {

		providerStarted <- struct{}{}
		<-ctx.Done()

		return ProviderResult{Outcome: OutcomeRetry}, ctx.Err()
	}
	idle := &idleDeliveryStore{
		SQLStore: h.store,
		idle:     make(chan struct{}, 2),
	}
	h.server.store = idle
	h.server.now = time.Now
	ctx, cancel := context.WithCancel(t.Context())
	deliveryDone := make(chan error, 1)
	sourceDone := make(chan struct{})
	go func() {
		deliveryDone <- h.server.Run(ctx)
	}()
	go func() {
		defer close(sourceDone)
		h.server.runFeedSource(ctx, h.store, "producer")
	}()
	t.Cleanup(func() {
		cancel()
		require.NoError(t, <-deliveryDone)
		<-sourceDone
	})
	// Release immediately after a periodic empty pass, leaving almost a
	// full second before the next fallback tick. No scheduling sleep is
	// used.
	for range 2 {
		select {
		case <-idle.idle:
		case <-time.After(3 * time.Second):
			t.Fatal("delivery worker did not reach its idle wait")
		}
	}
	close(available)
	select {
	case <-ackStarted:
	case <-time.After(time.Second):
		t.Fatal("ingress did not commit before ACK")
	}
	pending, err := h.store.database.Queries.CountPendingJobs(t.Context())
	require.NoError(t, err)
	require.EqualValues(t, 1, pending)
	select {
	case <-providerStarted:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("delivery waited for the ticker or blocked remote ACK")
	}
}

// TestSourceIdlePullPacing keeps producer long-waits as the idle pacing while
// retaining a backoff for a replica that cannot acquire the source lease.
func TestSourceIdlePullPacing(t *testing.T) {
	h, ingress, envelope := ingressHarness(t)
	pulls := make(chan *pb.PullRequest, 2)
	producer := &feedRPCFixture{source: ingress.source, event: envelope}
	producer.pull = func(ctx context.Context, request *pb.PullRequest) (
		*pb.PullResponse, error) {

		pulls <- request
		timer := time.NewTimer(
			time.Duration(request.WaitTimeoutMs) *
				time.Millisecond,
		)
		defer timer.Stop()
		select {
		case <-timer.C:
			return &pb.PullResponse{
				Status: &pb.Status{
					Ok: true,
				}, NextCursor: request.Cursor,
			}, nil

		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	startFeedRPCFixture(t, producer)
	started := time.Now()
	_, err := h.database.Queries.ClaimSource(
		t.Context(), sqlc.ClaimSourceParams{
			ID: "producer", ClaimToken: "other-replica",
			LeaseUntil: started.
				Add(600 * time.Millisecond).
				UnixNano(),
			NowNano: started.UnixNano(),
		},
	)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	server := &Server{
		wake: make(chan struct{}, 1),
		log:  slog.New(slog.DiscardHandler),
	}
	go func() {
		defer close(done)
		server.runFeedSource(ctx, h.store, "producer")
	}()
	t.Cleanup(func() { cancel(); <-done })
	select {
	case request := <-pulls:
		require.EqualValues(t, 4000, request.WaitTimeoutMs)
		require.GreaterOrEqual(
			t, time.Since(started), 900*time.Millisecond,
		)

	case <-time.After(3 * time.Second):
		t.Fatal(
			"source did not retry after the competing lease " +
				"expired",
		)
	}
	first := time.Now()
	select {
	case <-pulls:
		require.GreaterOrEqual(
			t, time.Since(first), 3900*time.Millisecond,
		)
		require.Less(t, time.Since(first), 4700*time.Millisecond)

	case <-time.After(4700 * time.Millisecond):
		t.Fatal("an extra idle sleep followed the empty long-wait")
	}
	cancelled := time.Now()
	cancel()
	select {
	case <-done:
		require.Less(t, time.Since(cancelled), 500*time.Millisecond)

	case <-time.After(500 * time.Millisecond):
		t.Fatal("cancellation did not interrupt the long-wait")
	}
}
