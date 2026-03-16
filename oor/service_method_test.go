package oor

import (
	"testing"

	"github.com/lightninglabs/darepo-client/rpc/oorpb"
	"github.com/lightninglabs/darepo/clientconn"
	"github.com/stretchr/testify/require"
)

// TestOORServiceMethodAlignment verifies that OOR response messages'
// ServiceMethod() returns the correct (Service, Method) pair for
// client-side EventRouter dispatch. The Method must match the
// oorpb.Method* constants used in client-side route registration.
func TestOORServiceMethodAlignment(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		msg           clientconn.ClientMessage
		expectService string
		expectMethod  string
	}{
		{
			name: "SubmitOORResponse",
			msg: &SubmitOORResponse{
				clientID: "test",
			},
			expectService: oorpb.ServiceName,
			expectMethod:  oorpb.MethodSubmitPackage,
		},
		{
			name: "FinalizeOORResponse",
			msg: &FinalizeOORResponse{
				clientID: "test",
			},
			expectService: oorpb.ServiceName,
			expectMethod:  oorpb.MethodFinalizePackage,
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
