package mailboxpull

import (
	"context"
	"errors"
	"testing"
	"time"

	mailboxpb "github.com/lightninglabs/wavelength/mailbox/pb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// fastBackoff returns a backoff config that essentially eliminates sleeps so
// tests don't add wall-time to the suite.
func fastBackoff() BackoffConfig {
	return BackoffConfig{
		BaseDelay: time.Microsecond,
		MaxDelay:  time.Microsecond,
	}
}

// pullStep is one step in a scripted pull sequence: either an error or a
// response is returned to the caller.
type pullStep struct {
	err  error
	resp *mailboxpb.PullResponse
}

// scriptedEdge implements MailboxServiceClient by consuming a scripted
// sequence of Pull outcomes, recording the cursor of every request so tests
// can assert that retries reuse the caller's cursor instead of rewinding.
type scriptedEdge struct {
	script  []pullStep
	cursors []uint64
	calls   int

	// onPull is invoked at the start of every Pull call, before the
	// scripted step is consumed. Tests use this to drive side effects
	// like ctx cancellation between attempts — fields like calls and
	// cursors are already updated when the hook runs, so the hook can
	// inspect which attempt is in flight.
	onPull func()
}

func (e *scriptedEdge) Send(context.Context, *mailboxpb.SendRequest,
	...grpc.CallOption) (*mailboxpb.SendResponse, error) {

	return nil, nil
}

func (e *scriptedEdge) Pull(_ context.Context, req *mailboxpb.PullRequest,
	_ ...grpc.CallOption) (*mailboxpb.PullResponse, error) {

	e.cursors = append(e.cursors, req.GetCursor())
	idx := e.calls
	e.calls++
	if e.onPull != nil {
		e.onPull()
	}
	if idx >= len(e.script) {

		// Exhausted script: behave like a permanently flapping
		// endpoint so callers that don't manage ctx will hit ctx
		// timeout rather than panic.
		return nil, status.Error(codes.Unavailable, "exhausted")
	}
	step := e.script[idx]

	return step.resp, step.err
}

func (e *scriptedEdge) AckUpTo(context.Context, *mailboxpb.AckUpToRequest,
	...grpc.CallOption) (*mailboxpb.AckUpToResponse, error) {

	return nil, nil
}

// TestPullWithRetrySucceedsFirstTry verifies the happy path: a single
// successful Pull is forwarded verbatim with no retry.
func TestPullWithRetrySucceedsFirstTry(t *testing.T) {
	t.Parallel()

	want := &mailboxpb.PullResponse{NextCursor: 42}
	edge := &scriptedEdge{script: []pullStep{{resp: want}}}

	resp, err := PullWithRetry(
		t.Context(), edge, &mailboxpb.PullRequest{
			Cursor: 7,
		},
		fastBackoff(),
		nil,
	)
	require.NoError(t, err)
	require.Same(t, want, resp)
	require.Equal(t, 1, edge.calls)
	require.Equal(t, []uint64{7}, edge.cursors)
}

// TestPullWithRetryRetriesTransientErrors verifies that a transient
// Unavailable error is retried until success, and the cursor is preserved
// across attempts.
func TestPullWithRetryRetriesTransientErrors(t *testing.T) {
	t.Parallel()

	want := &mailboxpb.PullResponse{NextCursor: 99}
	edge := &scriptedEdge{
		script: []pullStep{
			{
				err: status.Error(codes.Unavailable, "rst1"),
			},
			{
				err: status.Error(codes.Unavailable, "rst2"),
			},
			{
				err: status.Error(codes.Unavailable, "rst3"),
			},
			{
				resp: want,
			},
		},
	}

	resp, err := PullWithRetry(
		t.Context(), edge, &mailboxpb.PullRequest{
			Cursor: 17,
		},
		fastBackoff(),
		nil,
	)
	require.NoError(t, err)
	require.Same(t, want, resp)
	require.Equal(t, 4, edge.calls)
	require.Equal(
		t, []uint64{17, 17, 17, 17}, edge.cursors,
		"cursor must be preserved across retries",
	)
}

// TestPullWithRetryRespectsCtxCancel verifies that a cancelled ctx causes the
// helper to return ctx.Err() promptly even while the endpoint keeps failing.
func TestPullWithRetryRespectsCtxCancel(t *testing.T) {
	t.Parallel()

	edge := &scriptedEdge{
		script: []pullStep{
			{
				err: status.Error(codes.Unavailable, "rst"),
			},
		},
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := PullWithRetry(
		ctx, edge, &mailboxpb.PullRequest{
			Cursor: 1,
		},
		fastBackoff(),
		nil,
	)
	require.ErrorIs(t, err, context.Canceled)
}

// TestPullWithRetryReturnsCtxErrAfterCancel verifies that if the ctx is
// cancelled between Pull attempts (i.e. after edge.Pull has returned a
// transport error but before the helper schedules its backoff sleep), the
// helper returns ctx.Err() rather than the underlying transport error. This
// pins the asymmetry where ctx cancellation must take precedence: callers
// further up the stack treat ctx.Canceled as a normal early exit, while a
// transport error would be wrapped and surfaced as a swap failure.
func TestPullWithRetryReturnsCtxErrAfterCancel(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	transportErr := errors.New("transport boom")

	// Drive cancellation from inside the mock Pull callback so the helper
	// actually executes Pull, observes the transport error, and *then*
	// finds ctx done on its post-Pull re-check. Cancelling before the
	// call would just trip the opening ctx.Err() guard, which is the
	// same code path as TestPullWithRetryRespectsCtxCancel.
	edge := &scriptedEdge{
		script: []pullStep{
			{
				err: transportErr,
			},
		},
		onPull: func() {
			cancel()
		},
	}

	_, err := PullWithRetry(
		ctx, edge, &mailboxpb.PullRequest{
			Cursor: 1,
		},
		fastBackoff(),
		nil,
	)
	require.ErrorIs(
		t, err, context.Canceled,
		"ctx error must take precedence over transport error",
	)

	// The helper must have invoked Pull exactly once: the post-Pull
	// ctx.Err() check should bail before a retry attempt fires.
	require.Equal(
		t, 1, edge.calls,
		"helper must reach the post-Pull ctx re-check, not retry",
	)
}

// TestRetryDelayClampsToMax verifies the exponential backoff is capped at
// MaxDelay regardless of attempt count.
func TestRetryDelayClampsToMax(t *testing.T) {
	t.Parallel()

	cfg := BackoffConfig{
		BaseDelay: 100 * time.Millisecond,
		MaxDelay:  500 * time.Millisecond,
	}

	// At attempt=20, raw 2^19 * 100ms is wildly larger than 500ms; the
	// helper must clamp. Jitter is in [0.5, 1.0), so the upper bound is
	// MaxDelay.
	for i := 0; i < 100; i++ {
		d := RetryDelay(cfg, 20)
		require.LessOrEqual(t, d, cfg.MaxDelay)
		require.GreaterOrEqual(t, d, cfg.MaxDelay/2)
	}
}

// TestRetryDelayUsesDefaults verifies that a zero-value BackoffConfig falls
// back to package defaults rather than spinning with no delay.
func TestRetryDelayUsesDefaults(t *testing.T) {
	t.Parallel()

	d := RetryDelay(BackoffConfig{}, 1)
	// First attempt: base * 2^0 = base = 200ms, * jitter in [0.5, 1.0).
	require.GreaterOrEqual(t, d, defaultBaseDelay/2)
	require.LessOrEqual(t, d, defaultBaseDelay)
}
