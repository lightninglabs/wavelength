package waved

import (
	"testing"

	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestArkChannelMailboxErrorsUseStatusCodes verifies malformed and unavailable
// requests do not collapse to gRPC Unknown.
func TestArkChannelMailboxErrorsUseStatusCodes(t *testing.T) {
	t.Parallel()

	rpcServer := &RPCServer{}
	_, err := rpcServer.ExportOORRecoveryPackage(
		t.Context(), &waverpc.ExportOORRecoveryPackageRequest{},
	)
	require.Equal(t, codes.Unavailable, status.Code(err))
}
