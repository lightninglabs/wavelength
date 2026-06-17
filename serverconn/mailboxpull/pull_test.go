package mailboxpull

import (
	"context"
	"errors"
	"testing"
	"time"

	mailboxpb "github.com/lightninglabs/darepo-client/mailbox/pb"
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

// TestPullWithRetry exercises the retry helper across its happy path,
// transient-error retry, and the two ctx-cancellation precedence paths
// (cancel before the first call vs. cancel mid-flight after a transport
// error). Cancellation must always win over an in-flight transport error.
func TestPullWithRetry(t *testing.T) {
	t.Parallel()

	transientErr := status.Error(codes.Unavailable, "rst")
	want := &mailboxpb.PullResponse{NextCursor: 99}

	// cancelMode controls how (if at all) the ctx is cancelled relative to
	// the Pull attempts.
	type cancelMode int
	const (
		// noCancel leaves the ctx live for the whole call.
		noCancel cancelMode = iota

		// cancelBefore cancels prior to the first Pull, tripping the
		// opening ctx.Err() guard.
		cancelBefore

		// cancelMidFlight cancels from inside the first Pull so the
		// helper observes a transport error and then finds ctx done on
		// its post-Pull re-check.
		cancelMidFlight
	)

	tests := []struct {
		name        string
		script      []pullStep
		cursor      uint64
		cancel      cancelMode
		wantResp    *mailboxpb.PullResponse
		wantErr     error
		wantCalls   int
		wantCursors []uint64
	}{
		{
			name: "succeeds first try",
			script: []pullStep{
				{
					resp: want,
				},
			},
			cursor:    7,
			wantResp:  want,
			wantCalls: 1,
			wantCursors: []uint64{
				7,
			},
		},
		{
			name: "retries transient errors preserving cursor",
			script: []pullStep{
				{
					err: transientErr,
				},
				{
					err: transientErr,
				},
				{
					err: transientErr,
				},
				{
					resp: want,
				},
			},
			cursor:    17,
			wantResp:  want,
			wantCalls: 4,
			wantCursors: []uint64{
				17,
				17,
				17,
				17,
			},
		},
		{
			name: "ctx cancelled before first call",
			script: []pullStep{
				{
					err: transientErr,
				},
			},
			cursor:    1,
			cancel:    cancelBefore,
			wantErr:   context.Canceled,
			wantCalls: 0,
		},
		{
			name: "ctx cancel wins over transport error",
			script: []pullStep{
				{
					err: errors.New("transport boom"),
				},
			},
			cursor:    1,
			cancel:    cancelMidFlight,
			wantErr:   context.Canceled,
			wantCalls: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			edge := &scriptedEdge{script: tc.script}
			switch tc.cancel {
			case noCancel:
				// ctx stays live for the whole call.

			case cancelBefore:
				cancel()

			case cancelMidFlight:
				edge.onPull = cancel
			}

			resp, err := PullWithRetry(
				ctx, edge, &mailboxpb.PullRequest{
					Cursor: tc.cursor,
				},
				fastBackoff(),
				nil,
			)

			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
			} else {
				require.NoError(t, err)
				require.Same(t, tc.wantResp, resp)
			}
			if tc.wantCalls > 0 {
				require.Equal(t, tc.wantCalls, edge.calls)
			}
			if tc.wantCursors != nil {
				require.Equal(t, tc.wantCursors, edge.cursors)
			}
		})
	}
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
