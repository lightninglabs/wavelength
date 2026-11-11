package notifyrelay

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"math"
	"time"

	pb "github.com/lightninglabs/wavelength/mailbox/pb"
	"github.com/lightninglabs/wavelength/notification"
	"github.com/lightninglabs/wavelength/notifyrelay/relaydb/sqlc"
	"google.golang.org/grpc"
)

// feedServiceName is the fixed event service on a reserved subscriber mailbox.
const feedServiceName = "mailbox.v1.WakeFeedService"

// ErrFeedSource hides source responses and credentials from diagnostics. A bad
// envelope or unsupported version stops progress rather than acknowledging an
// obligation this binary cannot interpret.
var ErrFeedSource = errors.New("notification feed source unavailable")

// feedMailbox is the read-only subscriber transport. No network Send method is
// exposed here. The production connection adds TLS, RPC admission and R proof.
type feedMailbox interface {
	// Pull reads one ordered prefix without acknowledging it.
	Pull(context.Context, *pb.PullRequest,
		...grpc.CallOption) (*pb.PullResponse, error)

	// AckUpTo retires only a durably committed prefix.
	AckUpTo(context.Context, *pb.AckUpToRequest,
		...grpc.CallOption) (*pb.AckUpToResponse, error)
}

// feedIngress runs a bounded step under one leased source owner. SQL persists
// every local delivery obligation before the corresponding remote ACK.
type feedIngress struct {
	store   *SQLStore
	source  *feedSource
	mailbox feedMailbox
}

// step flushes a retained ACK even if the next Pull would be empty. That ACK
// lets the producer release its outstanding marker and emit pending N+1.
func (f *feedIngress) step(parent context.Context) (bool, error) {
	ctx, cancel := context.WithTimeout(parent, 20*time.Second)
	defer cancel()
	token, err := randomSecret(16)
	if err != nil {
		return false, err
	}
	var state sqlc.RelaySource
	err = f.store.database.Write(ctx, func(q *sqlc.Queries) error {
		var claimErr error
		now := time.Now()
		state, claimErr = q.ClaimSource(ctx, sqlc.ClaimSourceParams{
			ID: f.source.config.ID, ClaimToken: token,
			LeaseUntil: now.Add(claimLifetime).UnixNano(),
			NowNano:    now.UnixNano(),
		})

		return claimErr
	})
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	// Cancellation may prevent release. No detached cleanup escapes daemon
	// ownership; another replica can take over when the bounded lease
	// expires.
	defer func() {
		_ = f.store.database.Queries.ReleaseSource(
			ctx, sqlc.ReleaseSourceParams{
				ID:         state.ID,
				ClaimToken: token,
			},
		)
	}()
	if err := f.flushAck(ctx, state, token); err != nil {
		return false, err
	}
	pullCtx, pullCancel := context.WithTimeout(ctx, 5*time.Second)
	response, err := f.mailbox.Pull(pullCtx, &pb.PullRequest{
		MailboxId: f.source.mailboxID, Cursor: uint64(state.Cursor),
		MaxEnvelopes: 64, WaitTimeoutMs: 1000,
	})
	pullCancel()
	if err != nil {
		return false, ErrFeedSource
	}
	events, err := f.validateBatch(response, state.Cursor)
	if err != nil {
		return false, err
	}
	if len(events) == 0 {
		return false, nil
	}
	next := int64(response.NextCursor)
	if err := f.commitBatch(ctx, state, token, next, events); err != nil {
		return false, err
	}
	state.Cursor = next
	state.AckPending = true
	if err := f.flushAck(ctx, state, token); err != nil {
		return true, err
	}

	return true, nil
}

// flushAck clears only the exact committed prefix under the current claim.
// Losing an ACK response retains pending state for restart/retry.
func (f *feedIngress) flushAck(ctx context.Context, state sqlc.RelaySource,
	token string) error {

	if !state.AckPending {
		return nil
	}
	ackCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	response, err := f.mailbox.AckUpTo(ackCtx, &pb.AckUpToRequest{
		MailboxId: f.source.mailboxID, Cursor: uint64(state.Cursor),
	})
	cancel()
	if err != nil || response == nil || !response.GetStatus().GetOk() {
		return ErrFeedSource
	}

	return f.store.database.Write(ctx, func(q *sqlc.Queries) error {
		changed, err := q.ClearSourceAck(ctx, sqlc.ClearSourceAckParams{
			ID: state.ID, ClaimToken: token, Cursor: state.Cursor,
			NowNano: time.Now().UnixNano(),
		})
		if err != nil {
			return err
		}
		if changed != 1 {
			return ErrFeedSource
		}

		return nil
	})
}

// validateBatch accepts an ordered v1 feed prefix from the configured source.
// Sequence gaps are legal after producer expiry/revocation. Empty responses
// never advance a cursor. Malformed or unsupported feed data remains unacked.
func (f *feedIngress) validateBatch(response *pb.PullResponse, cursor int64) (
	[]*pb.WakeFeedEvent, error) {

	if response == nil || !response.GetStatus().GetOk() ||
		len(response.Envelopes) > 64 {
		return nil, ErrFeedSource
	}
	events := make([]*pb.WakeFeedEvent, 0, len(response.Envelopes))
	prior := uint64(cursor)
	version := pb.MailboxProtocolVersionV1
	for _, envelope := range response.Envelopes {
		if envelope == nil || envelope.EventSeq == 0 ||
			envelope.EventSeq < uint64(cursor) ||
			(len(events) > 0 && envelope.EventSeq <= prior) ||
			envelope.EventSeq >= math.MaxInt64 ||
			envelope.ProtocolVersion != version ||
			envelope.Sender != f.source.config.PublicKey ||
			envelope.Recipient != f.source.mailboxID ||
			envelope.GetRpc().GetKind() != pb.RpcMeta_KIND_EVENT ||
			envelope.GetRpc().GetService() != feedServiceName ||
			envelope.GetRpc().GetMethod() != "Wake" {
			return nil, ErrFeedSource
		}
		event := new(pb.WakeFeedEvent)
		if envelope.Body == nil ||
			envelope.Body.UnmarshalTo(event) != nil ||
			notification.ValidateFeedEvent(event) != nil {
			return nil, ErrFeedSource
		}
		events = append(events, event)
		prior = envelope.EventSeq
	}
	if len(events) > 0 && response.NextCursor != prior+1 {
		return nil, ErrFeedSource
	}

	return events, nil
}

// commitBatch fences a stale replica before any job write. Jobs, binding
// progress and the pending-ACK checkpoint share one serializable transaction.
func (f *feedIngress) commitBatch(ctx context.Context, prior sqlc.RelaySource,
	token string, next int64, events []*pb.WakeFeedEvent) error {

	return f.store.database.Write(ctx, func(q *sqlc.Queries) error {
		now := time.Now()
		claimed, err := q.GetClaimedSource(
			ctx, sqlc.GetClaimedSourceParams{
				ID:         prior.ID,
				ClaimToken: token,
				NowNano:    now.UnixNano(),
			},
		)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrFeedSource
		}
		if err != nil {
			return err
		}
		if claimed.Cursor != prior.Cursor {
			return ErrFeedSource
		}
		for _, event := range events {
			if err := f.admitEvent(ctx, q, event, now); err != nil {
				return err
			}
		}
		changed, err := q.CommitSourceCursor(
			ctx, sqlc.CommitSourceCursorParams{
				ID:          prior.ID,
				ClaimToken:  token,
				PriorCursor: prior.Cursor,
				NextCursor:  next,
				NowNano:     now.UnixNano(),
			},
		)
		if err != nil {
			return err
		}
		if changed != 1 {
			return ErrFeedSource
		}

		return nil
	})
}

// admitEvent resolves the publisher's random grant ID before using its token.
// A trusted producer may race the app's bind response, including token renewal.
// Admit that event under the already enrolled route and local class policy;
// correlation tokens cannot allocate a route or choose a destination.
func (f *feedIngress) admitEvent(ctx context.Context, q *sqlc.Queries,
	event *pb.WakeFeedEvent, now time.Time) error {

	sub, err := q.GetSubscription(ctx, event.GrantId)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	sub.ExpiresAt = min(sub.ExpiresAt, event.ExpiresAtUnix)
	if sub.ExpiresAt <= now.Unix() {
		return nil
	}
	classes := f.source.config.EventClasses
	if !sub.AlertsEnabled {
		classes &^= notification.FeedClassPayment
	}
	if classes&event.EventClasses == 0 {
		return nil
	}
	binding, err := q.GetFeedBinding(ctx, sqlc.GetFeedBindingParams{
		SourceID: f.source.config.ID, SubscriptionID: sub.ID,
	})
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil && bytes.Equal(binding.FeedToken, event.FeedToken) &&
		binding.LastGeneration >= int64(event.Generation) {
		return nil
	}
	// The authenticated ordered source stream is authoritative for its
	// token. Never wait for an app round trip while holding an aggregate
	// feed prefix.
	if err := q.PutFeedBinding(ctx, sqlc.PutFeedBindingParams{
		SourceID: f.source.config.ID, SubscriptionID: sub.ID,
		FeedToken: event.FeedToken, EventClasses: int32(classes),
		ExpiresAt: sub.ExpiresAt,
	}); err != nil {
		return err
	}
	if classes&event.EventClasses&notification.FeedClassWake != 0 {
		if err := enqueueSQLJobTx(
			ctx, q, sub, ScopeWake, "", now,
		); err != nil {
			return err
		}
	}
	if classes&event.EventClasses&notification.FeedClassPayment != 0 {
		if err := enqueueSQLJobTx(
			ctx, q, sub, ScopeAlert, event.PaymentEventId, now,
		); err != nil {
			return err
		}
	}
	changed, err := q.AdvanceFeedBinding(ctx, sqlc.AdvanceFeedBindingParams{
		SourceID: f.source.config.ID, SubscriptionID: sub.ID,
		FeedToken: event.FeedToken, Generation: int64(event.Generation),
		NowUnix: now.Unix(),
	})
	if err != nil {
		return err
	}
	if changed != 1 {
		return ErrFeedSource
	}

	return nil
}
