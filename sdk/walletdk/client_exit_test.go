package walletdk

import (
	"context"
	"errors"
	"testing"

	"github.com/lightninglabs/darepo-client/rpc/walletdkrpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

// stubWalletClient embeds the gRPC interface so calls to any method other
// than the ones explicitly stubbed fail loudly in tests.
type stubWalletClient struct {
	walletdkrpc.WalletServiceClient

	exit func(context.Context, *walletdkrpc.ExitRequest) (
		*walletdkrpc.ExitResponse, error)

	lastExit  *walletdkrpc.ExitRequest
	exitCalls int
}

func (s *stubWalletClient) Exit(ctx context.Context,
	req *walletdkrpc.ExitRequest, _ ...grpc.CallOption) (
	*walletdkrpc.ExitResponse, error) {

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

// newExitTestClient assembles a walletdk.Client backed by a stub wallet RPC
// client so Exit can be exercised without standing up a real daemon.
func newExitTestClient(wallet walletdkrpc.WalletServiceClient) *Client {
	return &Client{
		wallet:    wallet,
		canWallet: true,
	}
}

// errExitWalletRPC is the sentinel a stub returns to verify wallet RPC
// failures propagate without client-side fallback.
var errExitWalletRPC = errors.New("wallet rpc failed")

// TestExit exercises the SDK's Exit decision tree: cooperative leave,
// forced unilateral unroll, local validation rejections, and wallet RPC
// error propagation. Rows are data-only; the shared runner builds a stub
// that either returns the row's response or fails loudly when the row
// expects no RPC at all.
func TestExit(t *testing.T) {
	t.Parallel()

	coop := walletdkrpc.ExitMode_EXIT_MODE_COOPERATIVE
	unilateral := walletdkrpc.ExitMode_EXIT_MODE_UNILATERAL

	// resp/respErr drive the stub: a nil resp with no respErr means the
	// RPC must not be invoked. wantErr/wantErrIs imply a nil result; the
	// want* result fields and wantOnchain/wantForceAck assert the success
	// projection and the request echoed onto the wallet RPC.
	tests := []struct {
		name         string
		req          ExitRequest
		resp         *walletdkrpc.ExitResponse
		respErr      error
		wantErr      string
		wantErrIs    error
		wantPath     ExitPath
		wantCoop     bool
		wantQueued   []string
		wantCreated  bool
		wantActorID  string
		wantOnchain  string
		wantForceAck string
		wantCalls    int
	}{
		{
			name: "default cooperative",
			req: ExitRequest{
				Outpoint: testExitOutpoint,
			},
			resp: &walletdkrpc.ExitResponse{
				Mode: coop,
				QueuedOutpoints: []string{
					testExitOutpoint,
				},
			},
			wantPath: ExitPathCooperative,
			wantCoop: true,
			wantQueued: []string{
				testExitOutpoint,
			},
			wantCalls: 1,
		},
		{
			name: "cooperative destination",
			req: ExitRequest{
				Outpoint:    testExitOutpoint,
				Destination: testExitDestination,
			},
			resp: &walletdkrpc.ExitResponse{
				Mode: coop,
			},
			wantPath:    ExitPathCooperative,
			wantCoop:    true,
			wantOnchain: testExitDestination,
			wantCalls:   1,
		},
		{
			name: "forced unroll",
			req: ExitRequest{
				Outpoint:       testExitOutpoint,
				ForceUnrollAck: testForceUnrollAck,
			},
			resp: &walletdkrpc.ExitResponse{
				Mode:    unilateral,
				Created: true,
				ActorId: testActorID,
			},
			wantPath:     ExitPathUnilateral,
			wantCreated:  true,
			wantActorID:  testActorID,
			wantForceAck: testForceUnrollAck,
			wantCalls:    1,
		},
		{
			name: "destination with forced unroll",
			req: ExitRequest{
				Outpoint:       testExitOutpoint,
				Destination:    testExitDestination,
				ForceUnrollAck: testForceUnrollAck,
			},
			wantErr: "destination cannot be combined",
		},
		{
			name:    "empty outpoint",
			req:     ExitRequest{},
			wantErr: "outpoint is required",
		},
		{
			name: "wallet rpc error",
			req: ExitRequest{
				Outpoint: testExitOutpoint,
			},
			respErr:   errExitWalletRPC,
			wantErrIs: errExitWalletRPC,
			wantCalls: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			noRPC := tc.resp == nil && tc.respErr == nil
			wallet := &stubWalletClient{
				exit: func(context.Context,
					*walletdkrpc.ExitRequest) (
					*walletdkrpc.ExitResponse, error) {

					if noRPC {
						t.Fatal("wallet RPC must " + //nolint:ll
							"not be called")
					}

					return tc.resp, tc.respErr
				},
			}
			client := newExitTestClient(wallet)

			res, err := client.Exit(t.Context(), tc.req)

			switch {
			case tc.wantErr != "":
				require.ErrorContains(t, err, tc.wantErr)
				require.Nil(t, res)

			case tc.wantErrIs != nil:
				require.ErrorIs(t, err, tc.wantErrIs)
				require.Nil(t, res)

			default:
				require.NoError(t, err)
				require.Equal(t, tc.wantPath, res.Path)
				require.Equal(t, tc.wantCoop, res.Cooperative)
				require.Equal(
					t, tc.wantQueued, res.QueuedOutpoints,
				)
				require.Equal(t, tc.wantCreated, res.Created)
				require.Equal(t, tc.wantActorID, res.ActorID)
				require.Equal(
					t, testExitOutpoint,
					wallet.lastExit.GetOutpoint(),
				)
				require.Equal(
					t, tc.wantOnchain,
					wallet.lastExit.GetOnchainAddress(),
				)
				require.Equal(
					t, tc.wantForceAck,
					wallet.lastExit.GetForceUnrollAck(),
				)
			}

			require.Equal(t, tc.wantCalls, wallet.exitCalls)
		})
	}
}
