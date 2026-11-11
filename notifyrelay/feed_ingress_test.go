package notifyrelay

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"testing"
	"time"

	pb "github.com/lightninglabs/wavelength/mailbox/pb"
	"github.com/lightninglabs/wavelength/notification"
	"github.com/lightninglabs/wavelength/notifyrelay/relaydb/sqlc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
)

// feedTransport keeps crash-window assertions on the actual step boundary.
type feedTransport struct {
	pull func(context.Context, *pb.PullRequest) (*pb.PullResponse, error)
	ack  func(context.Context, *pb.AckUpToRequest) (
		*pb.AckUpToResponse, error)
}

// Pull delegates the exact subscriber request to the test producer.
func (f *feedTransport) Pull(ctx context.Context, r *pb.PullRequest,
	_ ...grpc.CallOption) (*pb.PullResponse, error) {

	return f.pull(ctx, r)
}

// AckUpTo delegates the cumulative ACK after local commit.
func (f *feedTransport) AckUpTo(ctx context.Context, r *pb.AckUpToRequest,
	_ ...grpc.CallOption) (*pb.AckUpToResponse, error) {

	return f.ack(ctx, r)
}

// ingressHarness creates a real local binding and producer event.
func ingressHarness(t *testing.T) (*sqlAuthHarness, *feedIngress,
	*pb.Envelope) {

	t.Helper()
	config := testSourceConfig(t, "producer")
	h := sourceHarness(t, config)
	id := h.sqlSubscription(t, "wallet")
	token := bytes.Repeat([]byte{9}, 32)
	require.NoError(
		t,
		h.bindTestFeed(
			t, id, FeedBindingRequest{
				SourceID: config.ID,
				FeedToken: base64.RawURLEncoding.EncodeToString(
					token,
				),
				EventClasses:  3,
				ExpiresAtUnix: h.now.Add(time.Hour).Unix(),
			},
		),
	)
	event := &pb.WakeFeedEvent{GrantId: id, FeedToken: token, Generation: 1,
		EventClasses: 3, PaymentEventId: fmt.Sprintf("%064x", 1),
		ExpiresAtUnix: h.now.Add(time.Hour).Unix()}
	body, err := anypb.New(event)
	require.NoError(t, err)
	source := h.store.sources[config.ID]
	envelope := &pb.Envelope{EventSeq: 1,
		ProtocolVersion: pb.MailboxProtocolVersionV1,
		Sender:          config.PublicKey, Recipient: source.mailboxID,
		Body: body,
		Rpc: &pb.RpcMeta{
			Kind:    pb.RpcMeta_KIND_EVENT,
			Service: "mailbox.v1.WakeFeedService",
			Method:  "Wake",
		}}

	return h, &feedIngress{store: h.store, source: source}, envelope
}

// feedBatch returns the producer's exact nonempty prefix watermark.
func feedBatch(envelope *pb.Envelope) *pb.PullResponse {
	return &pb.PullResponse{Status: &pb.Status{
		Ok: true,
	},
		Envelopes: []*pb.Envelope{
			envelope,
		}, NextCursor: envelope.EventSeq + 1}
}

// TestFeedIngressLostAckAndRestart proves local job/checkpoint commit precedes
// remote ACK, and a second database pool flushes retained ACK before Pull.
func TestFeedIngressLostAckAndRestart(t *testing.T) {
	h, f, envelope := ingressHarness(t)
	calls := 0
	f.mailbox = &feedTransport{
		pull: func(context.Context, *pb.PullRequest) (*pb.PullResponse,
			error) {

			return feedBatch(envelope), nil
		},
		ack: func(ctx context.Context, r *pb.AckUpToRequest) (
			*pb.AckUpToResponse, error) {

			require.EqualValues(t, 2, r.Cursor)
			state, err := h.database.Queries.GetSource(
				ctx, "producer",
			)
			require.NoError(t, err)
			require.True(t, state.AckPending)
			require.EqualValues(t, 2, state.Cursor)
			count, err := h.database.Queries.CountPendingJobs(
				ctx,
			)
			require.NoError(t, err)
			require.EqualValues(
				t, 2, count, "both obligations precede ACK",
			)
			calls++

			return nil, errors.New("response lost after producer " +
				"accepted ACK")
		},
	}
	worked, err := f.step(t.Context())
	require.True(t, worked)
	require.ErrorIs(t, err, ErrFeedSource)
	require.Equal(t, 1, calls)
	peerDB := h.open()
	defer func() {
		require.NoError(t, peerDB.Close())
	}()
	peer, err := NewSQLStore(t.Context(), peerDB, f.source.config)
	require.NoError(t, err)
	pending := true
	restarted := &feedIngress{store: peer, source: peer.sources["producer"],
		mailbox: &feedTransport{
			ack: func(ctx context.Context, r *pb.AckUpToRequest) (
				*pb.AckUpToResponse, error) {

				require.True(t, pending)
				require.EqualValues(t, 2, r.Cursor)
				pending = false

				return &pb.AckUpToResponse{
					Status: &pb.Status{
						Ok: true,
					},
				}, nil
			},
			pull: func(_ context.Context, r *pb.PullRequest) (
				*pb.PullResponse, error) {

				require.False(
					t, pending,
					"flush ACK before even an empty Pull",
				)
				require.EqualValues(t, 2, r.Cursor)

				return &pb.PullResponse{
					Status: &pb.Status{
						Ok: true,
					},
				}, nil
			},
		}}
	worked, err = restarted.step(t.Context())
	require.NoError(t, err)
	require.False(t, worked)
	state, err := peerDB.Queries.GetSource(t.Context(), "producer")
	require.NoError(t, err)
	require.False(t, state.AckPending)
	require.EqualValues(t, 2, state.Cursor)
	job, err := peerDB.Queries.GetJob(
		t.Context(), "wake/"+envelopeEvent(t, envelope).GrantId,
	)
	require.NoError(t, err)
	require.EqualValues(
		t, 1, job.Generation, "restart did not enqueue duplicate",
	)
}

// envelopeEvent decodes the test's typed payload for deliberate mutations.
func envelopeEvent(t *testing.T, envelope *pb.Envelope) *pb.WakeFeedEvent {
	t.Helper()
	event := new(pb.WakeFeedEvent)
	require.NoError(t, envelope.Body.UnmarshalTo(event))

	return event
}

// replaceFeedEvent replaces a typed body without editing a source-owned buffer.
func replaceFeedEvent(t *testing.T, envelope *pb.Envelope,
	event *pb.WakeFeedEvent) {

	t.Helper()
	body, err := anypb.New(event)
	require.NoError(t, err)
	envelope.Body = body
}

// claimFeedSource acquires a source lease with explicit time for takeover
// tests.
func claimFeedSource(t *testing.T, h *sqlAuthHarness, token string,
	now time.Time) sqlc.RelaySource {

	t.Helper()
	row, err := h.database.Queries.ClaimSource(
		t.Context(), sqlc.ClaimSourceParams{
			ID:         "producer",
			ClaimToken: token,
			NowNano:    now.UnixNano(),
			LeaseUntil: now.
				Add(claimLifetime).
				UnixNano(),
		},
	)
	require.NoError(t, err)

	return row
}

// TestFeedIngressReplicaFences prevents a losing or expired owner from writing
// jobs or clearing another owner's pending prefix.
func TestFeedIngressReplicaFences(t *testing.T) {
	h, f, envelope := ingressHarness(t)
	old := claimFeedSource(t, h, "old", h.now.Add(-2*claimLifetime))
	current := claimFeedSource(t, h, "new", time.Now())
	event := envelopeEvent(t, envelope)
	require.ErrorIs(
		t,
		f.commitBatch(
			t.Context(), old, "old", 2, []*pb.WakeFeedEvent{event},
		),
		ErrFeedSource,
	)
	count, err := h.database.Queries.CountPendingJobs(t.Context())
	require.NoError(t, err)
	require.Zero(t, count)
	require.NoError(
		t,
		f.commitBatch(
			t.Context(), current, "new", 2,
			[]*pb.WakeFeedEvent{event},
		),
	)
	changed, err := h.database.Queries.ClearSourceAck(
		t.Context(), sqlc.ClearSourceAckParams{
			ID:         "producer",
			ClaimToken: "old",
			Cursor:     2,
			NowNano: time.
				Now().
				UnixNano(),
		},
	)
	require.NoError(t, err)
	require.Zero(t, changed)
	// A duplicate batch cannot advance or re-enqueue the committed prefix.
	require.ErrorIs(
		t,
		f.commitBatch(
			t.Context(), current, "new", 2,
			[]*pb.WakeFeedEvent{event},
		),
		ErrFeedSource,
	)
	count, err = h.database.Queries.CountPendingJobs(t.Context())
	require.NoError(t, err)
	require.EqualValues(t, 2, count)
	f.mailbox = &feedTransport{pull: func(context.Context,
		*pb.PullRequest) (*pb.PullResponse, error) {

		t.Fatal("another live lease must prevent remote Pull")

		return nil, nil
	}}
	worked, err := f.step(t.Context())
	require.NoError(t, err)
	require.False(t, worked)
}

// TestFeedIngressQuotaRollback keeps both jobs and cursor uncommitted when
// replay capacity rejects the second obligation in the same event.
func TestFeedIngressQuotaRollback(t *testing.T) {
	h, f, envelope := ingressHarness(t)
	event := envelopeEvent(t, envelope)
	expiry := h.now.Add(dedupLifetime).Unix()
	params := sqlc.PutDedupParams{
		SubscriptionID: event.GrantId, InstallationID: "device",
		Scope: "alert", ExpiresAt: expiry,
	}
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
	f.mailbox = &feedTransport{
		pull: func(context.Context, *pb.PullRequest) (*pb.PullResponse,
			error) {

			return feedBatch(envelope), nil
		},
		ack: func(context.Context, *pb.AckUpToRequest) (
			*pb.AckUpToResponse, error) {

			t.Fatal("uncommitted capacity rejection must not ACK")

			return nil, nil
		},
	}
	worked, err := f.step(t.Context())
	require.False(t, worked)
	require.ErrorIs(t, err, ErrCapacity)
	state, err := h.database.Queries.GetSource(t.Context(), "producer")
	require.NoError(t, err)
	require.Zero(t, state.Cursor)
	require.False(t, state.AckPending)
	count, err := h.database.Queries.CountPendingJobs(t.Context())
	require.NoError(t, err)
	require.Zero(t, count, "wake insert rolled back with rejected payment")
	binding, err := h.database.Queries.GetFeedBinding(
		t.Context(), sqlc.GetFeedBindingParams{
			SourceID:       "producer",
			SubscriptionID: event.GrantId,
		},
	)
	require.NoError(t, err)
	require.Zero(t, binding.LastGeneration)
}

// TestFeedIngressDisabledAndStaleRoutes does not let a disabled destination or
// token renewal block the aggregate source's cumulative checkpoint.
func TestFeedIngressDisabledAndStaleRoutes(t *testing.T) {
	h, f, envelope := ingressHarness(t)
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
				owner.Disabled = true

				return saveSQLInstallation(
					t.Context(), q, owner,
				)
			},
		),
	)
	f.mailbox = &feedTransport{
		pull: func(context.Context, *pb.PullRequest) (*pb.PullResponse,
			error) {

			return feedBatch(envelope), nil
		},
		ack: func(context.Context, *pb.AckUpToRequest) (
			*pb.AckUpToResponse, error) {

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
	sent, err := h.store.claim(t.Context(), time.Now())
	require.NoError(t, err)
	require.Nil(t, sent, "signed token recovery owns re-enabling delivery")
	event := envelopeEvent(t, envelope)
	event.FeedToken = bytes.Repeat([]byte{5}, 32)
	event.Generation = 2
	replaceFeedEvent(t, envelope, event)
	envelope.EventSeq = 2
	worked, err = f.step(t.Context())
	require.NoError(t, err)
	require.True(t, worked)
	state, err := h.database.Queries.GetSource(t.Context(), "producer")
	require.NoError(t, err)
	require.EqualValues(t, 3, state.Cursor)
	job, err := h.database.Queries.GetJob(
		t.Context(), "wake/"+event.GrantId,
	)
	require.NoError(t, err)
	require.EqualValues(
		t, 2, job.Generation, "source token renewal retains work",
	)
}

// TestFeedIngressEnvelopeContract fails closed on unsupported or malformed
// prefixes while accepting legal sequence gaps and not advancing on empty.
func TestFeedIngressEnvelopeContract(t *testing.T) {
	_, f, envelope := ingressHarness(t)
	cases := []struct {
		name   string
		mutate func(*pb.Envelope)
	}{
		{
			"version",
			func(e *pb.Envelope) {
				e.ProtocolVersion = 2
			},
		},
		{
			"source",
			func(e *pb.Envelope) {
				e.Sender = "other"
			},
		},
		{
			"recipient",
			func(e *pb.Envelope) {
				e.Recipient = "other"
			},
		},
		{
			"kind",
			func(e *pb.Envelope) {
				e.Rpc.Kind = pb.RpcMeta_KIND_RESPONSE
			},
		},
		{
			"service",
			func(e *pb.Envelope) {
				e.Rpc.Service = "other"
			},
		},
		{
			"method",
			func(e *pb.Envelope) {
				e.Rpc.Method = "other"
			},
		},
		{
			"body",
			func(e *pb.Envelope) {
				e.Body = nil
			},
		},
		{
			"sequence",
			func(e *pb.Envelope) {
				e.EventSeq = math.MaxUint64
			},
		},
		{"classes", func(e *pb.Envelope) {
			event := envelopeEvent(t, e)
			event.EventClasses = 8
			replaceFeedEvent(t, e, event)
		}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			changed, ok := proto.Clone(envelope).(*pb.Envelope)
			require.True(t, ok)
			test.mutate(changed)
			_, err := f.validateBatch(feedBatch(changed), 0)
			require.ErrorIs(t, err, ErrFeedSource)
		})
	}
	envelope.EventSeq = 7
	_, err := f.validateBatch(feedBatch(envelope), 3)
	require.NoError(
		t, err, "revoked/expired producer rows leave legal gaps",
	)
	response := feedBatch(envelope)
	response.NextCursor++
	_, err = f.validateBatch(response, 3)
	require.ErrorIs(t, err, ErrFeedSource)
	_, err = f.validateBatch(feedBatch(envelope), 8)
	require.ErrorIs(t, err, ErrFeedSource, "no rewind")
	response.Envelopes = append(response.Envelopes, envelope)
	_, err = f.validateBatch(response, 3)
	require.ErrorIs(t, err, ErrFeedSource, "strictly increasing prefix")
	empty, err := f.validateBatch(
		&pb.PullResponse{
			Status:     &pb.Status{Ok: true},
			NextCursor: 999,
		},
		3,
	)
	require.NoError(t, err)
	require.Empty(t, empty, "empty watermark alone is not commit authority")
	require.EqualValues(
		t, notification.FeedClassWake|notification.FeedClassPayment,
		envelopeEvent(t, envelope).EventClasses,
	)
}

// TestFeedIngressBeforeAppBinding keeps the first wake when producer delivery
// beats the app's token handoff. Only an existing publisher route is eligible.
func TestFeedIngressBeforeAppBinding(t *testing.T) {
	h, f, envelope := ingressHarness(t)
	id := h.sqlSubscription(t, "fresh-route")
	event := envelopeEvent(t, envelope)
	event.GrantId = id
	replaceFeedEvent(t, envelope, event)
	f.mailbox = &feedTransport{
		pull: func(context.Context, *pb.PullRequest) (*pb.PullResponse,
			error) {

			return feedBatch(envelope), nil
		},
		ack: func(context.Context, *pb.AckUpToRequest) (
			*pb.AckUpToResponse, error) {

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
	binding, err := h.database.Queries.GetFeedBinding(
		t.Context(), sqlc.GetFeedBindingParams{
			SourceID:       "producer",
			SubscriptionID: id,
		},
	)
	require.NoError(t, err)
	require.Equal(t, event.FeedToken, binding.FeedToken)
	require.EqualValues(t, 1, binding.LastGeneration)
	job, err := h.database.Queries.GetJob(t.Context(), "wake/"+id)
	require.NoError(t, err)
	require.EqualValues(t, 1, job.Generation)
	// A delayed app handoff of that token preserves already committed
	// progress.
	require.NoError(
		t,
		h.bindTestFeed(
			t, id, FeedBindingRequest{
				SourceID: "producer",
				FeedToken: base64.RawURLEncoding.EncodeToString(
					event.FeedToken,
				),
				EventClasses:  3,
				ExpiresAtUnix: event.ExpiresAtUnix,
			},
		),
	)
	event.Generation = 2
	event.GrantId = base64.RawURLEncoding.EncodeToString(
		bytes.Repeat(
			[]byte{7}, 24,
		),
	)
	replaceFeedEvent(t, envelope, event)
	envelope.EventSeq = 2
	worked, err = f.step(t.Context())
	require.NoError(t, err)
	require.True(t, worked)
	count, err := h.database.Queries.CountPendingJobs(t.Context())
	require.NoError(t, err)
	require.EqualValues(
		t, 2, count, "an unknown route cannot allocate jobs",
	)
	state, err := h.database.Queries.GetSource(t.Context(), "producer")
	require.NoError(t, err)
	require.EqualValues(
		t, 3, state.Cursor, "unknown route does not block others",
	)
}

// TestFeedIngressCancellationOwnsLifetime leaves only a bounded SQL lease when
// cancellation interrupts Pull; no detached cleanup or remote ACK runs later.
func TestFeedIngressCancellationOwnsLifetime(t *testing.T) {
	h, f, _ := ingressHarness(t)
	ctx, cancel := context.WithCancel(t.Context())
	f.mailbox = &feedTransport{pull: func(callCtx context.Context,
		_ *pb.PullRequest) (*pb.PullResponse, error) {

		cancel()
		<-callCtx.Done()

		return nil, callCtx.Err()
	}, ack: func(context.Context, *pb.AckUpToRequest) (*pb.AckUpToResponse,
		error) {

		t.Fatal("cancelled uncommitted prefix cannot ACK")

		return nil, nil
	}}
	worked, err := f.step(ctx)
	require.ErrorIs(t, err, ErrFeedSource)
	require.False(t, worked)
	state, err := h.database.Queries.GetSource(t.Context(), "producer")
	require.NoError(t, err)
	require.Zero(t, state.Cursor)
	require.False(t, state.AckPending)
	require.LessOrEqual(
		t, state.LeaseUntil, time.Now().Add(claimLifetime).UnixNano(),
	)
}
