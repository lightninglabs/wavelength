package waved

import (
	"testing"

	"github.com/lightninglabs/wavelength/vhtlcrecovery/coordinator"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestRecoveryStatusLookupSelectors preserves recovery-ID queries while
// supporting indexed request-ID queries and rejecting ambiguous selectors.
func TestRecoveryStatusLookupSelectors(t *testing.T) {
	t.Parallel()
	service, err := coordinator.NewService(coordinator.ServiceConfig{
		Store: missingRecoveryStore{}, Unroll: noopUnrollRegistry{},
		Exiter: noopUnrollRegistry{},
	})
	require.NoError(t, err)
	rpc := &RPCServer{server: &Server{vhtlcRecovery: service}}
	for _, test := range []struct {
		name  string
		req   *waverpc.GetVHTLCRecoveryStatusRequest
		valid bool
	}{
		{
			"nil",
			nil,
			false,
		},
		{
			"empty",
			&waverpc.GetVHTLCRecoveryStatusRequest{},
			false,
		},
		{"both", &waverpc.GetVHTLCRecoveryStatusRequest{
			RecoveryId: "job", RequestId: "request",
		}, false},
		{"by id", &waverpc.GetVHTLCRecoveryStatusRequest{
			RecoveryId: "missing",
		}, true},
		{"by request", &waverpc.GetVHTLCRecoveryStatusRequest{
			RequestId: "missing",
		}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			resp, err := rpc.GetVHTLCRecoveryStatus(
				t.Context(), test.req,
			)
			if !test.valid {
				require.Equal(
					t, codes.InvalidArgument,
					status.Code(err),
				)

				return
			}
			require.NoError(t, err)
			require.False(t, resp.GetFound())
		})
	}
}
