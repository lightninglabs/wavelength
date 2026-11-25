package waved

import (
	"context"
	"testing"
	"time"

	"github.com/lightninglabs/wavelength/lnruntime"
	mailboxpb "github.com/lightninglabs/wavelength/mailbox/pb"
	"github.com/lightninglabs/wavelength/serverconn"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// channelReconnectEdge loses the first connection and holds the next pull
// open until the channel runtime joins it during shutdown.
type channelReconnectEdge struct {
	okPullEdge
	pulled chan string
	joined chan struct{}
	lost   bool
}

// Pull records the mailbox actually polled by serverconn on each connection.
func (e *channelReconnectEdge) Pull(ctx context.Context,
	req *mailboxpb.PullRequest, _ ...grpc.CallOption) (
	*mailboxpb.PullResponse, error) {

	e.pulled <- req.MailboxId
	if !e.lost {
		e.lost = true

		return nil, status.Error(codes.Unavailable, "disconnected")
	}
	<-ctx.Done()
	close(e.joined)

	return nil, ctx.Err()
}

// TestArkChannelConnectorReconnectAndStop exercises the wallet's real
// durable connector through a failed pull, reconnect and process restart.
func TestArkChannelConnectorReconnectAndStop(t *testing.T) {
	_, deliveries, _, _, _ := newSendOORTestStores(t)
	s := &Server{cfg: DefaultConfig(), deliveryStore: deliveries}
	s.arkProtocolVersion.Store(1)
	const identity = "test-wallet"
	for range 2 {
		edge := &channelReconnectEdge{
			pulled: make(chan string, 2),
			joined: make(chan struct{}),
		}
		s.cfg.Swap.ArkChannelMailbox = edge
		cfg := s.arkChannelConnectorConfig(identity)
		require.Equal(t, identity, cfg.LocalMailboxID)
		require.Equal(
			t, lnruntime.ArkChannelHubMailboxID(identity),
			cfg.RemoteMailboxID,
		)
		cfg.RetryBaseDelay = time.Millisecond
		cfg.RetryMaxDelay = time.Millisecond
		rt, err := serverconn.NewRuntime(cfg)
		require.NoError(t, err)
		s.setArkChannelProcess(rt, &arkChannelControllerStub{}, nil)
		t.Cleanup(func() {
			require.NoError(
				t,
				s.stopArkChannelProcess(
					context.Background(),
				),
			)
		})
		require.NoError(t, rt.Start(t.Context()))
		for range 2 {
			select {
			case mailbox := <-edge.pulled:
				require.Equal(t, cfg.ReplyMailboxID, mailbox)
				require.Equal(
					t, lnruntime.ArkChannelClientMailboxID(
						identity,
					),
					mailbox,
				)

			case <-time.After(5 * time.Second):
				t.Fatal("channel connector did not reconnect")
			}
		}
		require.NoError(t, s.stopArkChannelProcess(t.Context()))
		select {
		case <-edge.joined:
		default:
			t.Fatal("channel shutdown did not join the active pull")
		}
		require.Nil(t, s.getArkChannelController())
	}
}
