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

	req *waverpc.RequestVirtualChannelIntentRequest
}

func (s *channelTestDaemon) RequestVirtualChannelIntent(_ context.Context,
	req *waverpc.RequestVirtualChannelIntentRequest) (
	*waverpc.RequestVirtualChannelIntentResponse, error) {

	s.req = req

	return &waverpc.RequestVirtualChannelIntentResponse{
		Status: "requested",
	}, nil
}

func TestChannelRequestUsesSingleAmount(t *testing.T) {
	server := &channelTestDaemon{}
	cmd, cleanup := newChannelTestRootCmd(t, server)
	defer cleanup()

	cmd.SetArgs([]string{
		"--no-tls", "ark", "channel", "request", "149000",
	})
	require.NoError(t, cmd.Execute())

	require.NotNil(t, server.req)
	require.Equal(t, int64(149000), server.req.CapacitySat)
	require.Equal(t, int64(150000), server.req.BackingAmountSat)
	require.True(t, server.req.Private)
	require.True(t, server.req.ZeroConf)
	require.True(t, server.req.RoundFunded)
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
