package wavewalletdk

import (
	"context"
	"errors"
	"testing"

	"github.com/lightninglabs/wavelength/rpc/wavewalletrpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

// stubWalletClient embeds the gRPC interface so calls to any method other
// than the ones explicitly stubbed fail loudly in tests.
type stubWalletClient struct {
	wavewalletrpc.WalletServiceClient

	exit func(context.Context, *wavewalletrpc.ExitRequest) (
		*wavewalletrpc.ExitResponse, error)

	lastExit  *wavewalletrpc.ExitRequest
	exitCalls int
}

func (s *stubWalletClient) Exit(ctx context.Context,
	req *wavewalletrpc.ExitRequest, _ ...grpc.CallOption) (
	*wavewalletrpc.ExitResponse, error) {

	s.exitCalls++
	s.lastExit = req

	return s.exit(ctx, req)
}

const (
	testExitOutpoint    = "abcdef0123456789:0"
	testExitDestination = "bcrt1qexample"
	testActorID         = "actor-xyz"
	testForceUnrollAck  = "I_KNOW_WHAT_I_AM_DOING"
)

// newExitTestClient assembles a wavewalletdk.Client backed by a stub wallet RPC
// client so Exit can be exercised without standing up a real daemon.
func newExitTestClient(wallet wavewalletrpc.WalletServiceClient) *Client {
	return &Client{
		wallet:    wallet,
		canWallet: true,
	}
}

// TestExitDefaultUsesWalletRPCCooperative verifies that an empty destination
// still calls wavewalletrpc.Exit and reports the daemon's cooperative mode.
func TestExitDefaultUsesWalletRPCCooperative(t *testing.T) {
	t.Parallel()

	wallet := &stubWalletClient{
		exit: func(_ context.Context, _ *wavewalletrpc.ExitRequest) (
			*wavewalletrpc.ExitResponse, error) {

			mode := wavewalletrpc.ExitMode_EXIT_MODE_COOPERATIVE

			return &wavewalletrpc.ExitResponse{
				Mode: mode,
				QueuedOutpoints: []string{
					testExitOutpoint,
				},
			}, nil
		},
	}
	client := newExitTestClient(wallet)

	res, err := client.Exit(t.Context(), ExitRequest{
		Outpoint: testExitOutpoint,
	})
	require.NoError(t, err)
	require.Equal(t, ExitPathCooperative, res.Path)
	require.True(t, res.Cooperative)
	require.Equal(t, []string{testExitOutpoint}, res.QueuedOutpoints)
	require.Equal(t, 1, wallet.exitCalls)
	require.Equal(t, testExitOutpoint, wallet.lastExit.GetOutpoint())
	require.Empty(t, wallet.lastExit.GetOnchainAddress())
	require.Empty(t, wallet.lastExit.GetForceUnrollAck())
}

// TestExitCooperativeDestinationPassesThrough confirms a caller-supplied
// destination is forwarded to the wallet RPC surface.
func TestExitCooperativeDestinationPassesThrough(t *testing.T) {
	t.Parallel()

	wallet := &stubWalletClient{
		exit: func(_ context.Context, _ *wavewalletrpc.ExitRequest) (
			*wavewalletrpc.ExitResponse, error) {

			mode := wavewalletrpc.ExitMode_EXIT_MODE_COOPERATIVE

			return &wavewalletrpc.ExitResponse{
				Mode: mode,
			}, nil
		},
	}
	client := newExitTestClient(wallet)

	res, err := client.Exit(t.Context(), ExitRequest{
		Outpoint:    testExitOutpoint,
		Destination: testExitDestination,
	})
	require.NoError(t, err)
	require.Equal(t, ExitPathCooperative, res.Path)
	require.Equal(
		t, testExitDestination, wallet.lastExit.GetOnchainAddress(),
	)
	require.Empty(t, wallet.lastExit.GetForceUnrollAck())
}

// TestExitForcedUnrollPassesAck confirms the SDK does not hide forced unroll:
// the acknowledgement is forwarded and unilateral mode is projected.
func TestExitForcedUnrollPassesAck(t *testing.T) {
	t.Parallel()

	wallet := &stubWalletClient{
		exit: func(_ context.Context, _ *wavewalletrpc.ExitRequest) (
			*wavewalletrpc.ExitResponse, error) {

			mode := wavewalletrpc.ExitMode_EXIT_MODE_UNILATERAL

			return &wavewalletrpc.ExitResponse{
				Mode:    mode,
				Created: true,
				ActorId: testActorID,
			}, nil
		},
	}
	client := newExitTestClient(wallet)

	res, err := client.Exit(t.Context(), ExitRequest{
		Outpoint:       testExitOutpoint,
		ForceUnrollAck: testForceUnrollAck,
	})
	require.NoError(t, err)
	require.Equal(t, ExitPathUnilateral, res.Path)
	require.True(t, res.Created)
	require.Equal(t, testActorID, res.ActorID)
	require.Equal(
		t, testForceUnrollAck, wallet.lastExit.GetForceUnrollAck(),
	)
}

// TestExitRejectsDestinationWithForcedUnroll mirrors the wallet RPC's
// mutual-exclusion rule before spending a round trip.
func TestExitRejectsDestinationWithForcedUnroll(t *testing.T) {
	t.Parallel()

	wallet := &stubWalletClient{
		exit: func(context.Context, *wavewalletrpc.ExitRequest) (
			*wavewalletrpc.ExitResponse, error) {

			t.Fatal("wallet RPC must not be called")

			return nil, nil
		},
	}
	client := newExitTestClient(wallet)

	res, err := client.Exit(t.Context(), ExitRequest{
		Outpoint:       testExitOutpoint,
		Destination:    testExitDestination,
		ForceUnrollAck: testForceUnrollAck,
	})
	require.ErrorContains(t, err, "destination cannot be combined")
	require.Nil(t, res)
	require.Zero(t, wallet.exitCalls)
}

// TestExitSurfacesWalletRPCError verifies wallet RPC failures are returned
// without client-side fallback.
func TestExitSurfacesWalletRPCError(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("wallet rpc failed")
	wallet := &stubWalletClient{
		exit: func(_ context.Context, _ *wavewalletrpc.ExitRequest) (
			*wavewalletrpc.ExitResponse, error) {

			return nil, sentinel
		},
	}
	client := newExitTestClient(wallet)

	res, err := client.Exit(t.Context(), ExitRequest{
		Outpoint: testExitOutpoint,
	})
	require.ErrorIs(t, err, sentinel)
	require.Nil(t, res)
}

// TestExitRejectsEmptyOutpoint confirms local validation still rejects an
// unusable request before any RPC is attempted.
func TestExitRejectsEmptyOutpoint(t *testing.T) {
	t.Parallel()

	wallet := &stubWalletClient{
		exit: func(context.Context, *wavewalletrpc.ExitRequest) (
			*wavewalletrpc.ExitResponse, error) {

			t.Fatal("wallet RPC must not be called")

			return nil, nil
		},
	}
	client := newExitTestClient(wallet)

	res, err := client.Exit(t.Context(), ExitRequest{})
	require.ErrorContains(t, err, "outpoint is required")
	require.Nil(t, res)
	require.Zero(t, wallet.exitCalls)
}
