package rounds

import (
	"testing"

	"github.com/lightninglabs/darepo-client/rpc/roundpb"
	"github.com/lightninglabs/darepo/clientconn"
	"github.com/stretchr/testify/require"
)

// TestServiceMethodAlignment verifies that each client-facing outbox
// message's ServiceMethod() returns the correct (Service, Method) pair
// for client-side EventRouter dispatch. The expected values reference
// the roundpb.Method* constants directly — the same constants the
// client-side route registration uses — so a mismatch between server
// and client becomes a test failure rather than a silent dispatch drop.
func TestServiceMethodAlignment(t *testing.T) {
	t.Parallel()

	// Each test case maps an outbox message type to its expected
	// routing key. The Method must match the roundpb constant used
	// in client/darepod/server.go route registration.
	tests := []struct {
		name          string
		msg           clientconn.ClientMessage
		expectService string
		expectMethod  string
	}{
		{
			name: "ClientErrorResp",
			msg: &ClientErrorResp{
				Client: "test",
			},
			expectService: roundpb.ServiceName,
			expectMethod:  roundpb.MethodError,
		},
		{
			name: "ClientSuccessResp",
			msg: &ClientSuccessResp{
				Client: "test",
			},
			expectService: roundpb.ServiceName,
			expectMethod:  roundpb.MethodJoinAck,
		},
		{
			name: "ClientAwaitingInputSigsResp",
			msg: &ClientAwaitingInputSigsResp{
				Client: "test",
			},
			expectService: roundpb.ServiceName,
			expectMethod:  roundpb.MethodAwaitingInputSigs,
		},
		{
			name: "ClientVTXOAggNonces",
			msg: &ClientVTXOAggNonces{
				Client: "test",
			},
			expectService: roundpb.ServiceName,
			expectMethod:  roundpb.MethodAggNonces,
		},
		{
			name: "ClientVTXOAggSigs",
			msg: &ClientVTXOAggSigs{
				Client: "test",
			},
			expectService: roundpb.ServiceName,
			expectMethod:  roundpb.MethodAggSigs,
		},
		{
			name: "ClientBatchInfo",
			msg: &ClientBatchInfo{
				Client: "test",
			},
			expectService: roundpb.ServiceName,
			expectMethod:  roundpb.MethodBatchInfo,
		},
		{
			name: "ClientRoundFailedResp",
			msg: &ClientRoundFailedResp{
				Client: "test",
			},
			expectService: roundpb.ServiceName,
			expectMethod:  roundpb.MethodRoundFailed,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sm := tc.msg.ServiceMethod()

			require.Equal(
				t, tc.expectService, sm.Service, "Service "+
					"mismatch for %s", tc.name,
			)

			require.Equal(
				t, tc.expectMethod, sm.Method, "Method "+
					"mismatch for %s", tc.name,
			)
		})
	}
}
