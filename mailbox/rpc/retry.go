package mailboxrpc

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math"
	mrand "math/rand/v2"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Retry defaults. The backoff numbers deliberately mirror the mailbox pull
// loop's defaults so every client-side retry surface backs off the same way
// when the operator is struggling.
const (
	// idempotencyKeyBytes is the entropy width of a minted idempotency
	// key. Sixteen bytes matches the width the transport used when it
	// minted keys itself, so operator-side dedup tables see no change in
	// key shape.
	idempotencyKeyBytes = 16

	// DefaultRetryAttempts caps how many times one logical RPC is sent.
	// The bound matters more than its exact value: an unbounded retry
	// loop against a shedding operator is the amplification we are trying
	// to remove.
	DefaultRetryAttempts = 4

	// DefaultRetryBaseDelay is the first backoff increment after a shed
	// request.
	DefaultRetryBaseDelay = 200 * time.Millisecond

	// DefaultRetryMaxDelay caps the exponential growth of the backoff.
	DefaultRetryMaxDelay = 5 * time.Second

	// DefaultMaxRetryAfter caps how long an operator-supplied retry-after
	// hint may hold a caller back.
	//
	// The hint is a number chosen by the other side of the connection, so
	// honoring it unbounded would let a buggy or hostile operator park
	// every client of the fleet for as long as it liked by naming an
	// absurd delay. Thirty seconds is comfortably past any honest token
	// bucket's refill interval and comfortably short of an outage, so a
	// hint above it is a bug or an attack either way.
	DefaultMaxRetryAfter = 30 * time.Second
)

// NewIdempotencyKey mints the idempotency key for a single logical RPC.
//
// The key is random rather than a digest of the request payload because the
// operator uses it to collapse retries of one logical operation into one unit
// of work. Two callers issuing byte-identical concurrent reads are still two
// logical requests, and a payload digest would fold them into one, so entropy
// is the only thing that keeps distinct requests distinct while still letting
// a retry of the same request be recognized.
func NewIdempotencyKey() (string, error) {
	buf := make([]byte, idempotencyKeyBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate idempotency key: %w", err)
	}

	return hex.EncodeToString(buf), nil
}

// RetryPolicy bounds how hard a caller re-issues one logical mailbox RPC after
// the operator explicitly sheds it. The zero value selects the package
// defaults, so callers with no tuning opinion can pass RetryPolicy{} without
// producing a tight retry loop.
type RetryPolicy struct {
	// MaxAttempts caps the total number of sends, including the first.
	// A non-positive value selects DefaultRetryAttempts.
	MaxAttempts int

	// BaseDelay is the first backoff increment. A non-positive value
	// selects DefaultRetryBaseDelay.
	BaseDelay time.Duration

	// MaxDelay caps the exponential backoff delay. A non-positive value
	// selects DefaultRetryMaxDelay.
	MaxDelay time.Duration

	// MaxRetryAfter caps an operator-supplied retry-after hint, which is
	// otherwise preferred over the computed backoff. A non-positive value
	// selects DefaultMaxRetryAfter. Lower it when the caller must stay
	// responsive no matter how long the operator asks it to wait.
	MaxRetryAfter time.Duration
}

// normalize replaces non-positive fields with their defaults so the retry loop
// never has to re-check them.
func (p RetryPolicy) normalize() RetryPolicy {
	if p.MaxAttempts <= 0 {
		p.MaxAttempts = DefaultRetryAttempts
	}
	if p.BaseDelay <= 0 {
		p.BaseDelay = DefaultRetryBaseDelay
	}
	if p.MaxDelay <= 0 {
		p.MaxDelay = DefaultRetryMaxDelay
	}
	if p.MaxRetryAfter <= 0 {
		p.MaxRetryAfter = DefaultMaxRetryAfter
	}

	return p
}

// retryDelay returns the jittered backoff before attempt, which is one-based.
// The formula matches the mailbox pull loop's:
//
//	min(base * 2^(attempt-1), max) * U[0.5, 1.0).
//
// The jitter is what keeps a fleet of clients shed by the same token bucket
// from waking together and recreating the burst that got them shed. It uses
// non-cryptographic randomness because retry timing is not security
// sensitive.
func (p RetryPolicy) retryDelay(attempt int) time.Duration {
	delay := float64(p.BaseDelay) * math.Pow(2, float64(attempt-1))
	if delay > float64(p.MaxDelay) {
		delay = float64(p.MaxDelay)
	}

	return time.Duration(delay * (0.5 + mrand.Float64()*0.5)) //nolint:gosec
}

// backoffFor returns how long to wait before the attempt following the given
// one-based attempt, preferring the operator's retry-after hint over the
// locally computed backoff. It must be called on a normalized policy.
//
// The hint is better information than anything this side can compute: it
// comes from the very token bucket that shed the request, so a caller that
// honors it comes back exactly when it can be served rather than guessing,
// returning early, and getting shed again. Guessing is expensive here because
// an operator answers only the first shed in each window; every retry that
// lands too early is dropped in silence and teaches the caller nothing.
//
// It is still an untrusted number, so it is clamped to MaxRetryAfter. An
// absent, malformed, or non-positive hint falls through to the jittered
// exponential backoff, unchanged.
//
// The clamped hint is used as given, without the jitter the computed backoff
// carries, because the hint is derived from the caller's own per-client
// bucket rather than from a schedule shared across the fleet. Two callers
// honoring their own hints do not wake together, so there is no herd for
// jitter to break up.
func (p RetryPolicy) backoffFor(attempt int, err error) time.Duration {
	hint, ok := RetryAfter(err)
	if !ok || hint <= 0 {
		return p.retryDelay(attempt)
	}

	if hint > p.MaxRetryAfter {
		return p.MaxRetryAfter
	}

	return hint
}

// RetryAfter returns the retry-after hint the operator attached to err, and
// whether there was one at all.
//
// The hint rides as a standard google.rpc.RetryInfo detail on the gRPC
// status, which the mailbox already carries end to end in the response's
// error header, so reading it needs no new wire contract. Most errors carry
// no hint: only an operator that both sheds a request and can name a deadline
// annotates one.
func RetryAfter(err error) (time.Duration, bool) {
	st, ok := status.FromError(err)
	if !ok {
		return 0, false
	}

	for _, detail := range st.Details() {
		// A detail this side cannot resolve comes back as an error
		// rather than a message, so an unknown or corrupt entry is
		// skipped rather than trusted.
		info, ok := detail.(*errdetails.RetryInfo)
		if !ok {
			continue
		}

		delay := info.GetRetryDelay()
		if delay == nil || !delay.IsValid() {
			continue
		}

		return delay.AsDuration(), true
	}

	return 0, false
}

// IsShedError reports whether err is the operator's explicit "I am shedding
// this request" answer.
//
// A shed request is one the operator declined to serve but still answered, so
// it is the one failure worth waiting out and re-issuing. Every other failure
// either needs no retry or is not ours to fix. Note that a silently dropped
// request is indistinguishable from a slow one and surfaces as a deadline, not
// as this code.
func IsShedError(err error) bool {
	return status.Code(err) == codes.ResourceExhausted
}

// Retry runs one logical mailbox RPC to completion under policy, handing every
// attempt the same idempotency key.
//
// Minting the key here, at the logical-call boundary, rather than inside the
// transport is the whole point. The transport cannot tell a retry apart from a
// brand-new request, so a key minted per send makes the operator bill every
// attempt as fresh work and makes deduplication impossible. Bound to the
// logical call, the key lets the operator collapse a retry storm back into the
// single unit of work it really is.
//
// Only an explicit ResourceExhausted is retried, and only after a backoff:
// the operator's retry-after hint when the shed response names one, and a
// jittered exponential delay otherwise. A context that is cancelled or past
// its deadline is never retried: the caller has already given up, and another
// send would only queue work nobody is left to receive. Callers that want
// per-attempt deadlines should derive them inside call from the context Retry
// passes in.
func Retry(ctx context.Context, policy RetryPolicy,
	call func(context.Context, RPCOptions) error) error {

	return RetryWithKey(ctx, policy, "", call)
}

// RetryWithKey is Retry for callers that already hold the logical request's
// idempotency key, for example because the key is derived from a durable
// record so it survives a restart. Passing an empty key mints one.
//
// The key must not already be in flight elsewhere when this is called. Both
// the operator's deduplication and the transport's habit of defaulting the
// correlation ID to the key assume one live request per key, so two callers
// retrying concurrently under the same key would land two waiters on one
// correlation ID and let either one collect the other's answer. Nothing here
// enforces that, because every caller today re-issues sequentially from a
// single goroutine; a caller that wants concurrent attempts of the same
// logical request needs its own key per attempt-set.
func RetryWithKey(ctx context.Context, policy RetryPolicy, key string,
	call func(context.Context, RPCOptions) error) error {

	if key == "" {
		mintedKey, err := NewIdempotencyKey()
		if err != nil {
			return err
		}

		key = mintedKey
	}

	policy = policy.normalize()
	opts := RPCOptions{IdempotencyKey: key}

	var lastErr error
	for attempt := 1; attempt <= policy.MaxAttempts; attempt++ {
		// A caller that gave up between attempts must not put another
		// request on the wire; the operator would run it for nobody.
		if err := ctx.Err(); err != nil {
			if lastErr != nil {
				return lastErr
			}

			return err
		}

		lastErr = call(ctx, opts)
		if !IsShedError(lastErr) {
			return lastErr
		}

		if attempt == policy.MaxAttempts {
			break
		}

		if err := waitBackoff(
			ctx, policy.backoffFor(attempt, lastErr),
		); err != nil {
			return lastErr
		}
	}

	return lastErr
}

// waitBackoff pauses for delay, returning the context error early if the
// caller gives up while we are backing off.
func waitBackoff(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-timer.C:
		return nil

	case <-ctx.Done():
		return ctx.Err()
	}
}
