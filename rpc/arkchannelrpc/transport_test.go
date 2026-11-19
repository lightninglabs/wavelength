package arkchannelrpc

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/emptypb"
)

// TestChannelTransportSurface rejects removed demo calls and keeps peer
// controls outside the generated local gRPC service.
func TestChannelTransportSurface(t *testing.T) {
	t.Parallel()
	require.Equal(t, 1, File_ark_channel_proto.Services().Len())
	require.Equal(t, 2, File_ark_channel_peer_proto.Services().Len())
	require.Nil(
		t,
		File_ark_channel_proto.Messages().ByName("FundingWireEnvelope"),
	)
	for _, enum := range []protoreflect.Name{
		"FundingWireKind",
		"FundingWireMethod",
	} {
		require.Nil(t, File_ark_channel_proto.Enums().ByName(enum))
	}
	for _, method := range []string{"SendPayment", "ReceivePayment"} {
		require.Nil(
			t,
			File_ark_channel_proto.Services().Get(0).
				Methods().ByName(protoreflect.Name(method)),
		)
	}

	funding := File_ark_channel_peer_proto.Services().ByName(
		"ArkChannelFundingPeerService",
	)
	for _, method := range []protoreflect.Name{
		"RegisterPromotion", "SignBacking", "InstallBacking",
		"InstallRecoveryPackage", "ApplyChannelEvent",
	} {
		require.NotNil(t, funding.Methods().ByName(method))
	}
	for _, method := range []string{
		"CreateInvoice",
		"PayInvoice",
		"RegisterReceiveIntent",
		"FailReceiveIntent",
	} {
		require.Nil(
			t,
			funding.Methods().ByName(protoreflect.Name(method)),
		)
	}

	listener := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	RegisterArkChannelServiceServer(
		server, &UnimplementedArkChannelServiceServer{},
	)
	go func() {
		_ = server.Serve(listener)
	}()
	t.Cleanup(server.Stop)
	conn, err := grpc.NewClient(
		"passthrough:///channel-transport-test",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (
			net.Conn, error) {

			return listener.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(
			insecure.NewCredentials(),
		),
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	for _, method := range []string{
		"/arkchannelrpc.ArkChannelService/SendPayment",
		"/arkchannelrpc.ArkChannelService/ReceivePayment",
		"/arkchannelrpc.ArkChannelFundingPeerService/CreateInvoice",
		"/arkchannelrpc.ArkChannelFundingPeerService/PayInvoice",
		"/arkchannelrpc.ArkChannelFundingPeerService/GetPeerInfo",
		"/arkchannelrpc.ArkChannelPeerService/GetCooperativeCloseTerms",
	} {
		err := conn.Invoke(
			ctx, method, &emptypb.Empty{}, &emptypb.Empty{},
		)
		require.Equal(t, codes.Unimplemented, status.Code(err), method)
	}
}
