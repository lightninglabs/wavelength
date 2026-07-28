package mailboxrpc

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math"
	mrand "math/rand/v2"
	"time"

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
}

// DefaultRetryPolicy returns the production retry policy for logical mailbox
// RPCs.
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{
		MaxAttempts: DefaultRetryAttempts,
		BaseDelay:   DefaultRetryBaseDelay,
		MaxDelay:    DefaultRetryMaxDelay,
	}
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
// Only an explicit ResourceExhausted is retried, and only after a jittered
// backoff. A context that is cancelled or past its deadline is never retried:
// the caller has already given up, and another send would only queue work
// nobody is left to receive. Callers that want per-attempt deadlines should
// derive them inside call from the context Retry passes in.
func Retry(ctx context.Context, policy RetryPolicy,
	call func(context.Context, RPCOptions) error) error {

	key, err := NewIdempotencyKey()
	if err != nil {
		return err
	}

	return RetryWithKey(ctx, policy, key, call)
}

// RetryWithKey is Retry for callers that already hold the logical request's
// idempotency key, for example because the key is derived from a durable
// record so it survives a restart. Passing an empty key mints one.
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
			ctx, policy.retryDelay(attempt),
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
