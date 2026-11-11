package waved

import (
	"context"
	"errors"
	"testing"

	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/stretchr/testify/require"
)

// TestCustomOORSendHookCoversInProcessCalls ensures fault injection reaches the
// same boundary for embedded SDK calls and public gRPC calls, without blocking
// ordinary wallet sends or changing the on-chain recovery executor.
func TestCustomOORSendHookCoversInProcessCalls(t *testing.T) {
	t.Parallel()
	ready := make(chan struct{})
	close(ready)
	want := errors.New("injected custom-input outage")
	calls := 0
	cfg := DefaultConfig()
	cfg.CustomOORSendHook = func(_ context.Context,
		req *waverpc.SendOORRequest) error {

		calls++
		require.Len(t, req.GetCustomInputs(), 1)

		return want
	}
	rpc := &RPCServer{server: &Server{cfg: cfg, walletReady: ready}}
	_, err := rpc.SendOOR(
		t.Context(), &waverpc.SendOORRequest{
			CustomInputs: []*waverpc.CustomOORInput{
				{Outpoint: "funding:0"},
			},
		},
	)
	require.ErrorIs(t, err, want)
	require.Equal(t, 1, calls)

	// The malformed ordinary send fails input validation, not the test
	// gate.
	_, err = rpc.SendOOR(t.Context(), &waverpc.SendOORRequest{})
	require.Error(t, err)
	require.NotErrorIs(t, err, want)
	require.Equal(t, 1, calls)
}
