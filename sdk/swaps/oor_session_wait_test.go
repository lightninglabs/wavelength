package swaps

import (
	"context"
	"testing"
	"time"

	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/status"
)

// terminalWaitDaemon adds a scripted optional wait to the ordinary daemon fake.
type terminalWaitDaemon struct {
	*testDaemonConn
	wait func(context.Context, string, time.Duration) (
		*waverpc.OORSessionInfo, bool, error)
}

// WaitOORSession delegates to the scripted daemon wait.
func (d *terminalWaitDaemon) WaitOORSession(ctx context.Context, id string,
	delay time.Duration) (*waverpc.OORSessionInfo, bool, error) {

	return d.wait(ctx, id, delay)
}

// TestReceiveClaimTerminalWait verifies completion uses durable returned
// status, supported pending waits do not add a second delay, and older daemons
// retain the polling delay instead of making a busy loop.
func TestReceiveClaimTerminalWait(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{
		"completed",
		"pending",
		"old daemon",
		"canceled",
	} {
		t.Run(mode, func(t *testing.T) {
			h := newClaimSessionHarness(t, true)
			h.session.client.waitPollInterval = time.Hour
			h.session.client.daemon = &terminalWaitDaemon{
				testDaemonConn: h.daemon,
				wait: func(ctx context.Context, id string,
					delay time.Duration) (
					*waverpc.OORSessionInfo, bool, error) {

					require.Equal(t, "claim-session", id)
					require.Equal(t, time.Hour, delay)
					if mode == "canceled" {
						<-ctx.Done()

						return nil, false, status.
							FromContextError(
								ctx.Err(),
							).
							Err()
					}
					state := oorPending
					if mode == "completed" {
						state = oorCompleted
					}

					return &waverpc.OORSessionInfo{
							Status: state,
						},
						mode != "old daemon", nil
				},
			}
			ctx, cancel := context.WithTimeout(
				t.Context(), 50*time.Millisecond,
			)
			defer cancel()
			err := h.session.resolveClaimSession(ctx)
			if mode == "old daemon" || mode == "canceled" {
				require.ErrorIs(
					t, err, context.DeadlineExceeded,
				)
			} else {
				require.NoError(t, err)
			}
			if mode == "completed" {
				require.Equal(
					t, ReceiveStateCompleted,
					h.session.State(),
				)
			} else {
				require.Equal(
					t, ReceiveStateClaimInitiated,
					h.session.State(),
				)
			}
		})
	}
}
