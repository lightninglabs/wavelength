package rounds

import (
	"testing"

	"github.com/lightninglabs/darepo-client/rpc/roundpb"
	"github.com/lightninglabs/darepo/clientconn"
	"github.com/stretchr/testify/require"
)

// TestServiceMethodAlignment verifies that each client-facing outbox
// message's ServiceMethod() returns the correct (Service, Method) pair
// for client-side EventRouter dispatch. A mismatch silently drops
// messages with no compile-time error, so this test provides the
// safety net.
func TestServiceMethodAlignment(t *testing.T) {
	t.Parallel()

	// Each test case maps an outbox message type to its expected
	// routing key. The Method must match the constant used in
	// client_routes.go route registration.
	tests := []struct {
		name           string
		msg            clientconn.ClientMessage
		expectService  string
		expectMethod   string
	}{
		{
			name:          "ClientErrorResp",
			msg:           &ClientErrorResp{Client: "test"},
			expectService: roundpb.ServiceName,
			expectMethod:  MethodClientErrorResp,
		},
		{
			name:          "ClientSuccessResp",
			msg:           &ClientSuccessResp{Client: "test"},
			expectService: roundpb.ServiceName,
			expectMethod:  MethodClientSuccessResp,
		},
		{
			name: "ClientAwaitingInputSigsResp",
			msg: &ClientAwaitingInputSigsResp{
				Client: "test",
			},
			expectService: roundpb.ServiceName,
			expectMethod:  MethodClientAwaitingInputSigsResp,
		},
		{
			name:          "ClientVTXOAggNonces",
			msg:           &ClientVTXOAggNonces{Client: "test"},
			expectService: roundpb.ServiceName,
			expectMethod:  MethodClientVTXOAggNonces,
		},
		{
			name:          "ClientVTXOAggSigs",
			msg:           &ClientVTXOAggSigs{Client: "test"},
			expectService: roundpb.ServiceName,
			expectMethod:  MethodClientVTXOAggSigs,
		},
		{
			name:          "ClientBatchInfo",
			msg:           &ClientBatchInfo{Client: "test"},
			expectService: roundpb.ServiceName,
			expectMethod:  MethodClientBatchInfo,
		},
		{
			name: "ClientRoundFailedResp",
			msg: &ClientRoundFailedResp{
				Client: "test",
			},
			expectService: roundpb.ServiceName,
			expectMethod:  MethodClientRoundFailedResp,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sm := tc.msg.ServiceMethod()

			require.Equal(
				t, tc.expectService, sm.Service,
				"Service mismatch for %s", tc.name,
			)

			require.Equal(
				t, tc.expectMethod, sm.Method,
				"Method mismatch for %s", tc.name,
			)
		})
	}
}
