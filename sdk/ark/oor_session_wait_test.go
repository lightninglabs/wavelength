package ark

import (
	"context"
	"testing"
	"time"

	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

// oorWaitRPC captures the wait request and simulates old/new daemon responses.
type oorWaitRPC struct {
	waverpc.DaemonServiceClient
	request   *waverpc.GetOORSessionRequest
	supported bool
}

// GetOORSession records the additive wait field while returning durable status.
func (d *oorWaitRPC) GetOORSession(_ context.Context,
	req *waverpc.GetOORSessionRequest, _ ...grpc.CallOption) (
	*waverpc.GetOORSessionResponse, error) {

	d.request = req

	return &waverpc.GetOORSessionResponse{
		Session: &waverpc.OORSessionInfo{
			SessionId: req.SessionId,
		},
		TerminalWaitSupported: d.supported,
	}, nil
}

// TestWaitOORSessionCompatibility verifies an older daemon's default false
// capability survives the adapter and wait durations are bounded on the wire.
func TestWaitOORSessionCompatibility(t *testing.T) {
	t.Parallel()
	for _, supported := range []bool{false, true} {
		for _, delay := range []time.Duration{
			-time.Second,
			2 * time.Second,
			time.Hour,
		} {
			daemon := &oorWaitRPC{supported: supported}
			client := &Client{daemon: daemon}
			info, waited, err := client.WaitOORSession(
				t.Context(),
				"session", delay,
			)
			require.NoError(t, err)
			require.Equal(t, supported, waited)
			require.Equal(t, "session", info.SessionId)
			require.EqualValues(
				t,
				max(
					0,
					min(
						delay.Milliseconds(), 30_000,
					),
				),
				daemon.request.WaitForTerminalMs,
			)
		}
	}
}
