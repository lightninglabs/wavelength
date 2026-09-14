package waved

import (
	"testing"

	"github.com/lightninglabs/wavelength/arkchannel/oorbridge"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestChannelOORPreparationStatus verifies the mailbox enum is mapped without
// relying on the two packages retaining matching ordinal values.
func TestChannelOORPreparationStatus(t *testing.T) {
	t.Parallel()

	const (
		unspecified waverpc.ArkChannelOORPreparationStatus = 0
		absent      waverpc.ArkChannelOORPreparationStatus = 1
		pending     waverpc.ArkChannelOORPreparationStatus = 2
		prepared    waverpc.ArkChannelOORPreparationStatus = 3
		accepted    waverpc.ArkChannelOORPreparationStatus = 4
	)
	tests := []struct {
		internal oorbridge.PreparationStatus
		rpc      waverpc.ArkChannelOORPreparationStatus
	}{
		{
			internal: oorbridge.PreparationAbsent,
			rpc:      absent,
		},
		{
			internal: oorbridge.PreparationPending,
			rpc:      pending,
		},
		{
			internal: oorbridge.PreparationPrepared,
			rpc:      prepared,
		},
		{
			internal: oorbridge.PreparationAccepted,
			rpc:      accepted,
		},
		{
			internal: oorbridge.PreparationStatus(100),
			rpc:      unspecified,
		},
	}
	for _, test := range tests {
		require.Equal(
			t, test.rpc, channelOORPreparationStatus(test.internal),
		)
	}
}

// TestArkChannelMailboxErrorsUseStatusCodes verifies malformed and unavailable
// requests do not collapse to gRPC Unknown.
func TestArkChannelMailboxErrorsUseStatusCodes(t *testing.T) {
	t.Parallel()

	rpcServer := &RPCServer{}
	_, err := rpcServer.PrepareArkChannelOOR(
		t.Context(), &waverpc.PrepareArkChannelOORRequest{},
	)
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	_, err = rpcServer.AbortPreparedArkChannelOOR(
		t.Context(), &waverpc.AbortPreparedArkChannelOORRequest{},
	)
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	_, err = rpcServer.ExportOORRecoveryPackage(
		t.Context(), &waverpc.ExportOORRecoveryPackageRequest{},
	)
	require.Equal(t, codes.Unavailable, status.Code(err))
}
