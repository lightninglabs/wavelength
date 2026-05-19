package walletdk

import (
	"context"
	"errors"
	"testing"

	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/lightninglabs/darepo-client/rpc/walletrpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// stubDaemonClient embeds the gRPC interface (left nil) so calls to
// any method other than the ones we explicitly stub trip a nil-deref
// panic — the desired behavior for a test that should only exercise
// the methods Exit actually depends on.
type stubDaemonClient struct {
	daemonrpc.DaemonServiceClient

	leaveVTXOs func(ctx context.Context,
		req *daemonrpc.LeaveVTXOsRequest) (
		*daemonrpc.LeaveVTXOsResponse, error)

	listVTXOs func(ctx context.Context,
		req *daemonrpc.ListVTXOsRequest) (
		*daemonrpc.ListVTXOsResponse, error)

	leaveCalls int
	listCalls  int
}

func (s *stubDaemonClient) LeaveVTXOs(ctx context.Context,
	req *daemonrpc.LeaveVTXOsRequest, _ ...grpc.CallOption) (
	*daemonrpc.LeaveVTXOsResponse, error) {

	s.leaveCalls++

	return s.leaveVTXOs(ctx, req)
}

func (s *stubDaemonClient) ListVTXOs(ctx context.Context,
	req *daemonrpc.ListVTXOsRequest, _ ...grpc.CallOption) (
	*daemonrpc.ListVTXOsResponse, error) {

	s.listCalls++

	return s.listVTXOs(ctx, req)
}

// stubWalletClient mirrors stubDaemonClient for the wallet RPC
// surface; only Exit is needed.
type stubWalletClient struct {
	walletrpc.WalletServiceClient

	exit func(ctx context.Context, req *walletrpc.ExitRequest) (
		*walletrpc.ExitResponse, error)

	exitCalls int
}

func (s *stubWalletClient) Exit(ctx context.Context, req *walletrpc.ExitRequest,
	_ ...grpc.CallOption) (*walletrpc.ExitResponse, error) {

	s.exitCalls++

	return s.exit(ctx, req)
}

const (
	testExitOutpoint    = "abcdef0123456789:0"
	testExitDestination = "bcrt1qexample"
	testActorID         = "actor-xyz"
)

// newExitTestClient assembles a walletdk.Client backed by stub gRPC
// clients so the Exit decision tree can be exercised without
// standing up a real daemon.
func newExitTestClient(daemon daemonrpc.DaemonServiceClient,
	wallet walletrpc.WalletServiceClient) *Client {

	return &Client{
		daemon:    daemon,
		wallet:    wallet,
		canWallet: true,
	}
}

// listVTXOsStub is the signature of a stub for daemonrpc.ListVTXOs
// used by the H-3 ListVTXOs-guard tests. Aliased so callers can
// declare it without an 80-column-busting inline function literal
// type.
type listVTXOsStub = func(context.Context, *daemonrpc.ListVTXOsRequest) (
	*daemonrpc.ListVTXOsResponse, error)

// listVTXOsHit returns a ListVTXOs stub that echoes the supplied
// outpoint when queried for the supplied status filter, and an empty
// response otherwise. Used by the H-3 ListVTXOs-guard tests to
// model "the daemon already admitted the cooperative leave".
func listVTXOsHit(want daemonrpc.VTXOStatus, outpoint string) listVTXOsStub {
	hit := &daemonrpc.ListVTXOsResponse{
		Vtxos: []*daemonrpc.VTXO{{
			Outpoint: outpoint,
			Status:   want,
		}},
	}

	return func(_ context.Context, req *daemonrpc.ListVTXOsRequest) (
		*daemonrpc.ListVTXOsResponse, error) {

		if req.GetStatusFilter() == want {
			return hit, nil
		}

		return &daemonrpc.ListVTXOsResponse{}, nil
	}
}

// TestExitNoDestinationGoesStraightToUnilateral pins down the
// no-cooperative-attempt branch: when ExitRequest.Destination is
// empty the SDK calls walletrpc.Exit directly and reports
// ExitPathUnilateral without ever touching LeaveVTXOs / ListVTXOs.
func TestExitNoDestinationGoesStraightToUnilateral(t *testing.T) {
	t.Parallel()

	wallet := &stubWalletClient{
		exit: func(_ context.Context, _ *walletrpc.ExitRequest) (
			*walletrpc.ExitResponse, error) {

			return &walletrpc.ExitResponse{
				Created: true,
				ActorId: testActorID,
			}, nil
		},
	}
	daemon := &stubDaemonClient{}

	client := newExitTestClient(daemon, wallet)

	res, err := client.Exit(t.Context(), ExitRequest{
		Outpoint: testExitOutpoint,
	})
	require.NoError(t, err)
	require.Equal(t, ExitPathUnilateral, res.Path)
	require.True(t, res.Created)
	require.Equal(t, testActorID, res.ActorID)
	require.Empty(t, res.CooperativeError)
	require.Zero(
		t, daemon.leaveCalls,
		"no Destination must not invoke LeaveVTXOs",
	)
	require.Zero(
		t, daemon.listCalls, "no Destination must not invoke ListVTXOs",
	)
	require.Equal(t, 1, wallet.exitCalls)
}

// TestExitCooperativeSuccess covers the happy path: the daemon
// queues the outpoint, the SDK reports ExitPathCooperative and
// never falls through to Unroll.
func TestExitCooperativeSuccess(t *testing.T) {
	t.Parallel()

	daemon := &stubDaemonClient{
		leaveVTXOs: func(_ context.Context,
			_ *daemonrpc.LeaveVTXOsRequest) (
			*daemonrpc.LeaveVTXOsResponse, error) {

			return &daemonrpc.LeaveVTXOsResponse{
				QueuedOutpoints: []string{
					testExitOutpoint,
				},
				Status: "queued",
			}, nil
		},
	}
	wallet := &stubWalletClient{}

	client := newExitTestClient(daemon, wallet)

	res, err := client.Exit(t.Context(), ExitRequest{
		Outpoint:    testExitOutpoint,
		Destination: testExitDestination,
	})
	require.NoError(t, err)
	require.Equal(t, ExitPathCooperative, res.Path)
	require.True(t, res.Cooperative)
	require.Equal(t, []string{testExitOutpoint}, res.QueuedOutpoints)
	require.Equal(t, 1, daemon.leaveCalls)
	require.Zero(
		t, daemon.listCalls,
		"cooperative success must not probe ListVTXOs",
	)
	require.Zero(
		t, wallet.exitCalls,
		"cooperative success must not invoke unilateral Exit",
	)
}

// TestExitCooperativeEmptyQueuedFallsThrough pins down the H-2 fix:
// when the daemon returns nil error but an empty (or
// outpoint-missing) QueuedOutpoints set, the SDK treats the
// cooperative call as a failure and runs the full fallback
// pipeline.
func TestExitCooperativeEmptyQueuedFallsThrough(t *testing.T) {
	t.Parallel()

	daemon := &stubDaemonClient{
		leaveVTXOs: func(_ context.Context,
			_ *daemonrpc.LeaveVTXOsRequest) (
			*daemonrpc.LeaveVTXOsResponse, error) {

			// LeaveVTXOs surfaces per-outpoint wallet errors as
			// log lines rather than as a top-level error,
			// returning the partial QueuedOutpoints set. An
			// empty list signals our outpoint was dropped.
			return &daemonrpc.LeaveVTXOsResponse{
				QueuedOutpoints: nil,
				Status:          "queued",
			}, nil
		},
		listVTXOs: func(_ context.Context,
			_ *daemonrpc.ListVTXOsRequest) (
			*daemonrpc.ListVTXOsResponse, error) {

			return &daemonrpc.ListVTXOsResponse{}, nil
		},
	}
	wallet := &stubWalletClient{
		exit: func(_ context.Context, _ *walletrpc.ExitRequest) (
			*walletrpc.ExitResponse, error) {

			return &walletrpc.ExitResponse{
				Created: true,
				ActorId: testActorID,
			}, nil
		},
	}

	client := newExitTestClient(daemon, wallet)

	res, err := client.Exit(t.Context(), ExitRequest{
		Outpoint:    testExitOutpoint,
		Destination: testExitDestination,
	})
	require.NoError(t, err)
	require.Equal(t, ExitPathUnilateralFallback, res.Path)
	require.True(t, res.Created)
	require.Equal(t, testActorID, res.ActorID)
	require.Contains(
		t, res.CooperativeError, errCooperativeEmptyQueued.Error(),
		"H-2 empty-queued failure must carry the sentinel error",
	)
}

// TestExitCooperativeTerminalErrorNoFallback pins down the H-1 fix:
// caller-class gRPC errors (InvalidArgument, FailedPrecondition,
// NotFound, PermissionDenied) are returned verbatim instead of
// silently rerouting funds via the unilateral fallback path.
func TestExitCooperativeTerminalErrorNoFallback(t *testing.T) {
	t.Parallel()

	for _, code := range []codes.Code{
		codes.InvalidArgument,
		codes.FailedPrecondition,
		codes.NotFound,
		codes.PermissionDenied,
		codes.Unauthenticated,
	} {
		t.Run(code.String(), func(t *testing.T) {
			daemon := &stubDaemonClient{
				leaveVTXOs: func(_ context.Context,
					_ *daemonrpc.LeaveVTXOsRequest) (
					*daemonrpc.LeaveVTXOsResponse, error) {

					return nil, status.Error(
						code, "synthetic",
					)
				},
			}
			wallet := &stubWalletClient{}

			client := newExitTestClient(daemon, wallet)

			res, err := client.Exit(t.Context(), ExitRequest{
				Outpoint:    testExitOutpoint,
				Destination: testExitDestination,
			})
			require.Error(t, err)
			require.Nil(t, res)
			require.Equal(
				t, code, status.Code(err),
				"terminal codes must surface verbatim",
			)
			require.Zero(
				t, wallet.exitCalls, "terminal cooperative "+
					"error must NOT fall through to "+
					"unilateral",
			)
			require.Zero(
				t, daemon.listCalls, "terminal cooperative "+
					"error must NOT probe ListVTXOs",
			)
		})
	}
}

// TestExitCooperativeTransientErrorFallsBack covers the happy
// fallback path: a transport-class gRPC error from LeaveVTXOs +
// clean ListVTXOs probe → unilateral Unroll succeeds →
// ExitPathUnilateralFallback with the cooperative error attached.
func TestExitCooperativeTransientErrorFallsBack(t *testing.T) {
	t.Parallel()

	for _, code := range []codes.Code{
		codes.Unavailable,
		codes.DeadlineExceeded,
		codes.Aborted,
		codes.ResourceExhausted,
	} {
		t.Run(code.String(), func(t *testing.T) {
			cooperativeErr := status.Error(code, "synthetic")
			daemon := &stubDaemonClient{
				leaveVTXOs: func(_ context.Context,
					_ *daemonrpc.LeaveVTXOsRequest) (
					*daemonrpc.LeaveVTXOsResponse, error) {

					return nil, cooperativeErr
				},
				listVTXOs: func(_ context.Context,
					_ *daemonrpc.ListVTXOsRequest) (
					*daemonrpc.ListVTXOsResponse, error) {

					empty := &daemonrpc.ListVTXOsResponse{}

					return empty, nil
				},
			}
			wallet := &stubWalletClient{
				exit: func(_ context.Context,
					_ *walletrpc.ExitRequest) (
					*walletrpc.ExitResponse, error) {

					return &walletrpc.ExitResponse{
						Created: true,
						ActorId: testActorID,
					}, nil
				},
			}

			client := newExitTestClient(daemon, wallet)

			res, err := client.Exit(t.Context(), ExitRequest{
				Outpoint:    testExitOutpoint,
				Destination: testExitDestination,
			})
			require.NoError(t, err)
			require.Equal(
				t, ExitPathUnilateralFallback, res.Path,
			)
			require.True(t, res.Created)
			require.Equal(
				t, cooperativeErr.Error(), res.CooperativeError,
			)
			require.Equal(
				t, 2, daemon.listCalls, "H-3 guard must "+
					"probe both PendingForfeit and "+
					"Forfeiting before falling back",
			)
		})
	}
}

// TestExitListVTXOsGuardRefusesFallback pins down the H-3 fix: even
// when the cooperative gRPC call returned a retryable error, if
// ListVTXOs shows the outpoint already in PendingForfeit /
// Forfeiting the SDK refuses to fall back rather than racing the
// daemon's in-flight cooperative leave with an Unroll.
func TestExitListVTXOsGuardRefusesFallback(t *testing.T) {
	t.Parallel()

	for _, st := range []daemonrpc.VTXOStatus{
		daemonrpc.VTXOStatus_VTXO_STATUS_PENDING_FORFEIT,
		daemonrpc.VTXOStatus_VTXO_STATUS_FORFEITING,
	} {
		t.Run(st.String(), func(t *testing.T) {
			daemon := &stubDaemonClient{
				leaveVTXOs: func(_ context.Context,
					_ *daemonrpc.LeaveVTXOsRequest) (
					*daemonrpc.LeaveVTXOsResponse, error) {

					return nil, status.Error(
						codes.Canceled,
						"client gave up",
					)
				},
				listVTXOs: listVTXOsHit(st, testExitOutpoint),
			}
			wallet := &stubWalletClient{}

			client := newExitTestClient(daemon, wallet)

			// We use t.Context() here even though LeaveVTXOs
			// returned codes.Canceled: the synthetic error is
			// not driven by an actually-cancelled ctx, so the
			// M-6 short-circuit will not fire and the H-3
			// guard's ListVTXOs probe runs to completion.
			res, err := client.Exit(t.Context(), ExitRequest{
				Outpoint:    testExitOutpoint,
				Destination: testExitDestination,
			})
			require.Error(t, err)
			require.Nil(t, res)
			require.Contains(
				t, err.Error(),
				"daemon already admitted",
			)
			require.Zero(
				t, wallet.exitCalls, "H-3 guard must NOT "+
					"issue Unroll when the cooperative "+
					"attempt is in flight",
			)
		})
	}
}

// TestExitCtxCancelShortCircuits pins down the M-6 fix: if the
// caller's ctx is already cancelled by the time the cooperative
// attempt fails, the fallback is short-circuited rather than
// wasting another RPC against an expired ctx.
func TestExitCtxCancelShortCircuits(t *testing.T) {
	t.Parallel()

	daemon := &stubDaemonClient{
		leaveVTXOs: func(_ context.Context,
			_ *daemonrpc.LeaveVTXOsRequest) (
			*daemonrpc.LeaveVTXOsResponse, error) {

			return nil, status.Error(
				codes.Canceled, "client gave up",
			)
		},
	}
	wallet := &stubWalletClient{}

	client := newExitTestClient(daemon, wallet)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res, err := client.Exit(ctx, ExitRequest{
		Outpoint:    testExitOutpoint,
		Destination: testExitDestination,
	})
	require.Error(t, err)
	require.Nil(t, res)
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(
		t, daemon.listCalls,
		"ctx-cancelled short-circuit must not probe ListVTXOs",
	)
	require.Zero(
		t, wallet.exitCalls,
		"ctx-cancelled short-circuit must not call Unroll",
	)
}

// TestExitListVTXOsLookupFailureSurfaces verifies that a ListVTXOs
// probe failure is returned to the caller wrapped alongside the
// cooperative error rather than silently choosing the fallback
// path. This keeps the SDK from making a decision on stale state.
func TestExitListVTXOsLookupFailureSurfaces(t *testing.T) {
	t.Parallel()

	probeErr := errors.New("backend offline")
	daemon := &stubDaemonClient{
		leaveVTXOs: func(_ context.Context,
			_ *daemonrpc.LeaveVTXOsRequest) (
			*daemonrpc.LeaveVTXOsResponse, error) {

			return nil, status.Error(codes.Unavailable, "transport")
		},
		listVTXOs: func(_ context.Context,
			_ *daemonrpc.ListVTXOsRequest) (
			*daemonrpc.ListVTXOsResponse, error) {

			return nil, probeErr
		},
	}
	wallet := &stubWalletClient{}

	client := newExitTestClient(daemon, wallet)

	res, err := client.Exit(t.Context(), ExitRequest{
		Outpoint:    testExitOutpoint,
		Destination: testExitDestination,
	})
	require.Error(t, err)
	require.Nil(t, res)
	require.Contains(t, err.Error(), "admission probe failed")
	require.Zero(
		t, wallet.exitCalls, "probe failure must NOT proceed to Unroll",
	)
}
