package waveclicommands

import (
	"bytes"
	"context"
	"net"
	"testing"

	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

type channelTestDaemon struct {
	waverpc.UnimplementedDaemonServiceServer

	receiveReq *waverpc.RegisterReceiveChannelIntentRequest
	promoteReq *waverpc.OpenVirtualChannelRequest
}

func (s *channelTestDaemon) RegisterReceiveChannelIntent(_ context.Context,
	req *waverpc.RegisterReceiveChannelIntentRequest) (
	*waverpc.RegisterReceiveChannelIntentResponse, error) {

	s.receiveReq = req

	return &waverpc.RegisterReceiveChannelIntentResponse{
		Status: "requested",
	}, nil
}

func (s *channelTestDaemon) OpenVirtualChannel(_ context.Context,
	req *waverpc.OpenVirtualChannelRequest) (
	*waverpc.OpenVirtualChannelResponse, error) {

	s.promoteReq = req

	return &waverpc.OpenVirtualChannelResponse{Status: "active"}, nil
}

func TestChannelPromoteUsesSingleAmount(t *testing.T) {
	server := &channelTestDaemon{}
	cmd, cleanup := newChannelTestRootCmd(t, server)
	defer cleanup()

	cmd.SetArgs([]string{
		"--no-tls", "--no-macaroons", "ark", "channel", "promote",
		"149000",
	})
	require.NoError(t, cmd.Execute())

	require.NotNil(t, server.promoteReq)
	require.Equal(t, int64(149000), server.promoteReq.AmountSat)
	require.Len(t, server.promoteReq.IdempotencyKey, 64)
}

func TestChannelRequestUsesSingleAmount(t *testing.T) {
	server := &channelTestDaemon{}
	cmd, cleanup := newChannelTestRootCmd(t, server)
	defer cleanup()

	cmd.SetArgs([]string{
		"--no-tls", "--no-macaroons", "ark", "channel", "request",
		"149000",
	})
	require.NoError(t, cmd.Execute())

	require.NotNil(t, server.receiveReq)
	require.Equal(t, int64(149000), server.receiveReq.AmountSat)
	require.Len(t, server.receiveReq.IdempotencyKey, 64)
}

func TestChannelRequestRejectsInvalidAmount(t *testing.T) {
	cmd := newArkCmd()
	var stderr bytes.Buffer
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"channel", "request", "0"})

	err := cmd.Execute()
	require.Error(t, err)
	require.Contains(t, err.Error(), "amount-sat must be greater than zero")
}

func newChannelTestRootCmd(t *testing.T,
	server waverpc.DaemonServiceServer) (*cobra.Command, func()) {

	t.Helper()

	listener := bufconn.Listen(1024 * 1024)
	grpcServer := grpc.NewServer()
	waverpc.RegisterDaemonServiceServer(grpcServer, server)

	errChan := make(chan error, 1)
	go func() {
		errChan <- grpcServer.Serve(listener)
	}()

	prevDial := newDaemonClientConn
	newDaemonClientConn = func(string, ...grpc.DialOption) (
		*grpc.ClientConn, error) {

		return grpc.NewClient(
			"passthrough:///bufnet",
			grpc.WithContextDialer(func(context.Context, string) (
				net.Conn, error) {

				return listener.Dial()
			}),
			grpc.WithTransportCredentials(
				insecure.NewCredentials(),
			),
		)
	}

	cleanup := func() {
		newDaemonClientConn = prevDial
		grpcServer.Stop()
		_ = listener.Close()
		<-errChan
	}

	return NewRootCmd(), cleanup
}
