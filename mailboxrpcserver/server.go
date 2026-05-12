package mailboxrpcserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/btcsuite/btclog/v2"
	mailboxpb "github.com/lightninglabs/darepo-client/mailbox/pb"
	"github.com/lightninglabs/darepo/mailbox"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

const (
	// defaultPullLimit is the default maximum number of envelopes returned
	// by Pull when the request does not specify a limit.
	defaultPullLimit = 100

	// maxPullLimit is a hard upper bound on how many envelopes a
	// single Pull is allowed to return.
	maxPullLimit = 10_000

	// defaultNoWaitPullTimeout is the timeout applied when the request opts
	// out of long-polling by setting wait_timeout_ms = 0.
	//
	// This is a compromise: we want Pull to be non-blocking, but we still
	// need to allow the backing store a brief window to run at least one
	// query. Using a zero deadline can cause SQL-backed queries to fail
	// before they start.
	defaultNoWaitPullTimeout = 50 * time.Millisecond
)

// Server implements mailboxpb.MailboxServiceServer using a mailbox.Store.
type Server struct {
	// Required for forward compatibility.
	mailboxpb.UnimplementedMailboxServiceServer

	store mailbox.Store
	log   btclog.Logger
}

// New creates a new mailbox gRPC server backed by store.
func New(store mailbox.Store,
	log ...fn.Option[btclog.Logger]) (*Server, error) {

	if store == nil {
		return nil, fmt.Errorf("missing store")
	}

	logger := btclog.Disabled
	if len(log) > 0 {
		logger = log[0].UnwrapOr(btclog.Disabled)
	}

	return &Server{
		store: store,
		log:   logger,
	}, nil
}

// Send enqueues a single envelope into the recipient mailbox.
func (s *Server) Send(ctx context.Context, req *mailboxpb.SendRequest) (
	*mailboxpb.SendResponse, error) {

	if req == nil {
		return &mailboxpb.SendResponse{
			Status: invalidArgumentStatus("missing request"),
		}, nil
	}
	if req.Envelope == nil {
		return &mailboxpb.SendResponse{
			Status: invalidArgumentStatus("missing envelope"),
		}, nil
	}
	if req.Envelope.Recipient == "" {
		return &mailboxpb.SendResponse{
			Status: invalidArgumentStatus("missing recipient"),
		}, nil
	}
	if req.Envelope.MsgId == "" {
		return &mailboxpb.SendResponse{
			Status: invalidArgumentStatus("missing msg_id"),
		}, nil
	}

	s.log.DebugS(ctx, "Send RPC",
		slog.String("recipient", req.Envelope.Recipient),
		slog.String("msg_id", req.Envelope.MsgId),
	)

	_, err := s.store.Append(ctx, req.Envelope)
	if err != nil {
		var (
			envTooLarge *mailbox.ErrEnvelopeTooLarge
			mailboxFull *mailbox.ErrMailboxFull
		)
		if errors.As(err, &envTooLarge) ||
			errors.As(err, &mailboxFull) {
			return &mailboxpb.SendResponse{
				Status: resourceExhaustedStatus(
					err.Error(),
				),
			}, nil
		}

		return &mailboxpb.SendResponse{
			Status: internalStatus(
				fmt.Sprintf("append: %v", err),
			),
		}, nil
	}

	return &mailboxpb.SendResponse{
		Status: okStatus(),
	}, nil
}

// Pull returns a batch of envelopes and the updated cursor.
func (s *Server) Pull(ctx context.Context, req *mailboxpb.PullRequest) (
	*mailboxpb.PullResponse, error) {

	if req == nil {
		return &mailboxpb.PullResponse{
			Status: invalidArgumentStatus("missing request"),
		}, nil
	}
	if req.MailboxId == "" {
		return &mailboxpb.PullResponse{
			Status: invalidArgumentStatus("missing mailbox_id"),
		}, nil
	}

	limit, err := parsePullLimit(req.MaxEnvelopes)
	if err != nil {
		return &mailboxpb.PullResponse{
			Status: invalidArgumentStatus(err.Error()),
		}, nil
	}

	pullCtx, cancel := withPullTimeout(ctx, req.WaitTimeoutMs)
	defer cancel()

	envs, nextCursor, err := s.store.Pull(
		pullCtx, req.MailboxId, req.Cursor, limit,
	)
	if err != nil {
		if errors.Is(err, context.Canceled) ||
			errors.Is(err, context.DeadlineExceeded) {
			return &mailboxpb.PullResponse{
				Status:     okStatus(),
				Envelopes:  nil,
				NextCursor: req.Cursor,
			}, nil
		}

		return &mailboxpb.PullResponse{
			Status: internalStatus(
				fmt.Sprintf("pull: %v", err),
			),
		}, nil
	}

	s.log.DebugS(ctx, "Pull RPC",
		slog.String("mailbox_id", req.MailboxId),
		slog.Int("count", len(envs)),
		slog.Uint64("next_cursor", nextCursor),
	)

	return &mailboxpb.PullResponse{
		Status:     okStatus(),
		Envelopes:  envs,
		NextCursor: nextCursor,
	}, nil
}

// AckUpTo advances the mailbox ack cursor.
func (s *Server) AckUpTo(ctx context.Context, req *mailboxpb.AckUpToRequest) (
	*mailboxpb.AckUpToResponse, error) {

	if req == nil {
		return &mailboxpb.AckUpToResponse{
			Status: invalidArgumentStatus("missing request"),
		}, nil
	}
	if req.MailboxId == "" {
		return &mailboxpb.AckUpToResponse{
			Status: invalidArgumentStatus("missing mailbox_id"),
		}, nil
	}

	s.log.DebugS(ctx, "AckUpTo RPC",
		slog.String("mailbox_id", req.MailboxId),
		slog.Uint64("cursor", req.Cursor),
	)

	err := s.store.AckUpTo(ctx, req.MailboxId, req.Cursor)
	if err != nil {
		return &mailboxpb.AckUpToResponse{
			Status: internalStatus(
				fmt.Sprintf("ack: %v", err),
			),
		}, nil
	}

	return &mailboxpb.AckUpToResponse{
		Status: okStatus(),
	}, nil
}

// okStatus creates a successful mailbox status.
func okStatus() *mailboxpb.Status {
	return &mailboxpb.Status{
		Ok: true,
	}
}

// invalidArgumentStatus creates a mailbox status for invalid input.
func invalidArgumentStatus(msg string) *mailboxpb.Status {
	return &mailboxpb.Status{
		Ok:      false,
		Code:    "invalid_argument",
		Message: msg,
	}
}

// internalStatus creates a mailbox status for internal failures.
func internalStatus(msg string) *mailboxpb.Status {
	return &mailboxpb.Status{
		Ok:      false,
		Code:    "internal",
		Message: msg,
	}
}

// resourceExhaustedStatus creates a mailbox status for quota failures.
func resourceExhaustedStatus(msg string) *mailboxpb.Status {
	return &mailboxpb.Status{
		Ok:      false,
		Code:    "resource_exhausted",
		Message: msg,
	}
}

// parsePullLimit validates and bounds the requested Pull limit.
func parsePullLimit(maxEnvelopes uint32) (int, error) {
	if maxEnvelopes == 0 {
		return defaultPullLimit, nil
	}

	if maxEnvelopes > maxPullLimit {
		return 0, fmt.Errorf("max_envelopes exceeds limit")
	}

	return int(maxEnvelopes), nil
}

// withPullTimeout applies the requested wait timeout to ctx.
func withPullTimeout(ctx context.Context,
	waitTimeoutMs uint32) (context.Context, context.CancelFunc) {

	if waitTimeoutMs == 0 {
		return context.WithTimeout(ctx, defaultNoWaitPullTimeout)
	}

	return context.WithTimeout(
		ctx, time.Duration(waitTimeoutMs)*time.Millisecond,
	)
}
