package waved

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/serverconn/mailboxpull"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// connectAndNegotiateWithRetry runs connectAndNegotiate until the operator
// answers, retrying while it is unreachable.
//
// The wallet-dependent services, and every RPC that needs the wallet actor,
// start only after this succeeds. Giving up on the first failed attempt left a
// daemon started while the operator was briefly unreachable without those
// services for the rest of its life, even after the operator came back. The
// retry follows the shared mailbox backoff (200 ms base, 30 s cap) so the
// daemon backs off from an unreachable operator the same way its ingress loop
// does, and stops when the daemon shuts down.
func (s *Server) connectAndNegotiateWithRetry(ctx context.Context) (
	*arkVersionNegotiation, error) {

	return retryWhileOperatorUnreachable(
		ctx, mailboxpull.DefaultBackoffConfig(), s.connectAndNegotiate,
		func(attempt int, err error) {
			s.releaseOperatorClients(ctx)

			s.log.InfoS(ctx, "Ark server unreachable, retrying",
				slog.Int("attempt", attempt),
				btclog.Fmt("error", "%v", err),
			)
		},
	)
}

// releaseOperatorClients closes the operator connection a failed connect
// attempt opened, so the next attempt does not leak it.
func (s *Server) releaseOperatorClients(ctx context.Context) {
	cleanup := s.serverConnCleanup
	s.serverConnCleanup = nil
	s.mailboxClient = nil
	s.publishOperatorClients(&operatorClients{})

	if cleanup == nil {
		return
	}
	if err := cleanup(); err != nil {
		s.log.DebugS(ctx, "Closing operator connection of failed "+
			"attempt", btclog.Fmt("error", "%v", err))
	}
}

// retryWhileOperatorUnreachable runs attempt until it succeeds, fails for a
// reason that retrying cannot fix, or ctx ends. onRetry runs after each
// failed attempt that will be retried, with that attempt's number.
func retryWhileOperatorUnreachable[T any](ctx context.Context,
	backoff mailboxpull.BackoffConfig,
	attempt func(context.Context) (T, error),
	onRetry func(attempt int, err error)) (T, error) {

	var failures int
	for {
		result, err := attempt(ctx)
		if err == nil || !isOperatorUnreachable(err) {
			return result, err
		}

		onRetry(failures+1, err)
		mailboxpull.Sleep(ctx, backoff, &failures)

		if ctxErr := ctx.Err(); ctxErr != nil {
			var zero T

			return zero, fmt.Errorf("%w while retrying "+
				"unreachable operator: %v", ctxErr, err)
		}
	}
}

// isOperatorUnreachable reports whether err means the operator could not be
// reached or could not answer in time, which a later attempt may get past.
// Every other failure, such as an incompatible protocol version, is returned
// at once: retrying it would only hide a misconfiguration.
func isOperatorUnreachable(err error) bool {
	switch status.Code(err) {
	case codes.Unavailable, codes.DeadlineExceeded,
		codes.ResourceExhausted:
		return true

	default:
		return false
	}
}
