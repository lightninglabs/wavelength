package waved

import (
	"context"
	"testing"

	"github.com/lightninglabs/wavelength/arkchannel"
	"github.com/lightninglabs/wavelength/rpc/arkchannelrpc"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// arkChannelControllerStub records local RPC calls and returns a configured
// durable channel record.
type arkChannelControllerStub struct {
	record  arkchannel.Record
	err     error
	id      arkchannel.ID
	feeRate chainfee.SatPerKWeight
}

// RequestCooperativeClose records the parsed public request.
func (s *arkChannelControllerStub) RequestCooperativeClose(_ context.Context,
	id arkchannel.ID, feeRate chainfee.SatPerKWeight) (arkchannel.Record,
	error) {

	s.id = id
	s.feeRate = feeRate

	return s.record, s.err
}

// GetChannel returns the configured channel record.
func (s *arkChannelControllerStub) GetChannel(_ context.Context,
	id arkchannel.ID) (arkchannel.Record, error) {

	s.id = id

	return s.record, s.err
}

// TestArkChannelRPCRequestCooperativeClose verifies fixed-width ID parsing,
// fee defaults, and controller dispatch on the local authenticated RPC surface.
func TestArkChannelRPCRequestCooperativeClose(t *testing.T) {
	t.Parallel()

	id := arkchannel.ID{1, 2, 3}
	controller := &arkChannelControllerStub{
		record: arkchannel.Record{
			Snapshot: arkchannel.Snapshot{
				Terms: arkchannel.Terms{
					ID: id,
				},
				Phase: arkchannel.PhaseCoopClosing,
			},
			Revision: 4,
		},
	}
	server := &Server{arkChannelController: controller}
	rpcServer := &arkChannelRPCServer{server: server}

	response, err := rpcServer.RequestCooperativeClose(
		t.Context(), &arkchannelrpc.RequestCooperativeCloseRequest{
			ChannelId: id[:],
		},
	)
	require.NoError(t, err)
	require.Equal(t, id, controller.id)
	require.Equal(t, chainfee.FeePerKwFloor, controller.feeRate)
	require.Equal(t, id[:], response.GetChannel().GetChannelId())
	require.Equal(t, uint64(4), response.GetChannel().GetRevision())

	_, err = rpcServer.RequestCooperativeClose(
		t.Context(), &arkchannelrpc.RequestCooperativeCloseRequest{
			ChannelId:       id[:],
			FeeRateSatPerKw: uint64(chainfee.FeePerKwFloor - 1),
		},
	)
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	_, err = rpcServer.RequestCooperativeClose(
		t.Context(), &arkchannelrpc.RequestCooperativeCloseRequest{
			ChannelId: []byte{1},
		},
	)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

// TestArkChannelRPCUnavailable verifies the local service remains registered
// but fails explicitly when no compiled-in channel runtime was installed.
func TestArkChannelRPCUnavailable(t *testing.T) {
	t.Parallel()

	id := arkchannel.ID{1}
	rpcServer := &arkChannelRPCServer{server: &Server{}}

	_, err := rpcServer.GetChannel(
		t.Context(), &arkchannelrpc.GetChannelRequest{
			ChannelId: id[:],
		},
	)
	require.Equal(t, codes.Unavailable, status.Code(err))
}

var _ ArkChannelController = (*arkChannelControllerStub)(nil)
