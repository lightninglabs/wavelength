// Package mailboxpull provides shared retry+backoff primitives for mailbox
// pull loops. The same shape is needed by the persistent identity-mailbox
// ingress loop in the parent serverconn package and by per-swap event
// consumers in the SDK; this package factors that shape out so reliability
// semantics stay uniform across both consumers.
package mailboxpull

import (
	"context"
	"log/slog"
	"math"
	"math/rand/v2"
	"time"

	"github.com/btcsuite/btclog/v2"
	mailboxpb "github.com/lightninglabs/wavelength/mailbox/pb"
)

// Default backoff parameters mirror serverconn.DefaultConnectorConfig so the
// daemon ingress loop and any SDK pull loop back off identically when the
// shared mailbox endpoint is flapping.
const (
	defaultBaseDelay = 200 * time.Millisecond
	defaultMaxDelay  = 30 * time.Second
)

// BackoffConfig controls exponential backoff with jitter for retryable pull
// failures. The zero value selects the package defaults (200 ms base, 30 s
// cap) so callers that do not care about backoff tuning can pass
// BackoffConfig{} without producing a tight retry loop.
type BackoffConfig struct {
	// BaseDelay is the initial backoff increment. A non-positive value
	// selects the default (200 ms).
	BaseDelay time.Duration

	// MaxDelay caps the exponential backoff delay. A non-positive value
	// selects the default (30 s).
	MaxDelay time.Duration
}

// DefaultBackoffConfig returns the production defaults used by the serverconn
// ingress loop: 200 ms base, 30 s cap.
func DefaultBackoffConfig() BackoffConfig {
	return BackoffConfig{
		BaseDelay: defaultBaseDelay,
		MaxDelay:  defaultMaxDelay,
	}
}

// RetryDelay returns an exponential backoff duration with jitter, capped at
// cfg.MaxDelay. The formula is
//
//	min(base * 2^(attempt-1), max) * U[0.5, 1.0).
//
// The jitter uses non-cryptographic randomness because backoff timing is not
// security sensitive.
func RetryDelay(cfg BackoffConfig, attempt int) time.Duration {
	base := cfg.BaseDelay
	if base <= 0 {
		base = defaultBaseDelay
	}
	maxDelay := cfg.MaxDelay
	if maxDelay <= 0 {
		maxDelay = defaultMaxDelay
	}

	// Exponential backoff: base * 2^(attempt-1).
	delay := float64(base) * math.Pow(2, float64(attempt-1))
	if delay > float64(maxDelay) {
		delay = float64(maxDelay)
	}

	// Add jitter: multiply by a random factor in [0.5, 1.0). Spread
	// concurrent retries across the same endpoint so they do not
	// synchronize into a thundering herd.
	jitter := 0.5 + rand.Float64()*0.5 //nolint:gosec
	delay *= jitter

	return time.Duration(delay)
}

// Sleep increments *attempt and sleeps for the next backoff interval,
// respecting context cancellation. The attempt counter is owned by the
// caller so multiple pull cycles can share a single backoff schedule
// (i.e. successive failures grow the delay, while a success resets it).
func Sleep(ctx context.Context, cfg BackoffConfig, attempt *int) {
	*attempt++
	delay := RetryDelay(cfg, *attempt)

	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

// PullWithRetry calls edge.Pull, retrying transport errors with exponential
// backoff until the call succeeds or ctx is done. The cursor and other
// request fields are passed through verbatim — the helper does not mutate
// req — so the caller's cursor state is preserved across reconnects.
//
// On context cancellation the context error is returned, not the underlying
// transport error, so callers can distinguish "caller gave up" from "endpoint
// is flapping". A nil logger is treated as btclog.Disabled.
//
// Status-level failures (resp.Status non-nil with Ok=false) are returned to
// the caller in resp; this helper retries only on transport errors. The
// caller decides how to handle a status error — usually that means surfacing
// it to the caller of the outer Wait loop, since it indicates a deterministic
// protocol-level problem rather than a transient network blip.
func PullWithRetry(ctx context.Context, edge mailboxpb.MailboxServiceClient,
	req *mailboxpb.PullRequest, cfg BackoffConfig,
	log btclog.Logger) (*mailboxpb.PullResponse, error) {

	if log == nil {
		log = btclog.Disabled
	}

	var failCount int
	for {
		// Bail out before issuing a fresh Pull if the caller has
		// already cancelled. This avoids a needless round trip in the
		// common shutdown path.
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		resp, err := edge.Pull(ctx, req)
		if err == nil {
			return resp, nil
		}

		// Distinguish "we are shutting down" from "endpoint is
		// flapping" — callers further up the stack treat ctx
		// cancellation as a normal early exit, not a swap failure.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}

		log.WarnS(ctx, "Mailbox pull failed, retrying",
			err,
			slog.String("mailbox_id", req.GetMailboxId()),
			slog.Uint64("cursor", req.GetCursor()),
		)

		Sleep(ctx, cfg, &failCount)
	}
}
