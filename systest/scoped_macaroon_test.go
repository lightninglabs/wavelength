//go:build systest

package systest

import (
	"context"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	btcwalletrpc "github.com/btcsuite/btcwallet/rpc/walletrpc"
	"github.com/lightninglabs/wavelength/rpc/wavewalletrpc"
	"github.com/lightninglabs/wavelength/waved"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/lightningnetwork/lnd/macaroons"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

// scopedMacaroonWalletServer provides successful handlers behind the real
// waved authentication interceptor. The test is about the daemon's RPC
// boundary, not the wallet business logic exercised by other systests.
type scopedMacaroonWalletServer struct {
	wavewalletrpc.UnimplementedWalletServiceServer
	wavewalletrpc.UnimplementedWalletInspectionServiceServer
}

// Balance returns a successful empty response after authorization.
func (*scopedMacaroonWalletServer) Balance(context.Context,
	*wavewalletrpc.BalanceRequest) (*wavewalletrpc.BalanceResponse, error) {

	return &wavewalletrpc.BalanceResponse{}, nil
}

// List returns a successful empty response after authorization.
func (*scopedMacaroonWalletServer) List(context.Context,
	*wavewalletrpc.ListRequest) (*wavewalletrpc.ListResponse, error) {

	return &wavewalletrpc.ListResponse{}, nil
}

// PrepareSend returns a successful empty response after authorization.
func (*scopedMacaroonWalletServer) PrepareSend(context.Context,
	*wavewalletrpc.PrepareSendRequest) (*wavewalletrpc.PrepareSendResponse,
	error) {

	return &wavewalletrpc.PrepareSendResponse{}, nil
}

// Send returns a successful empty response after authorization.
func (*scopedMacaroonWalletServer) Send(context.Context,
	*wavewalletrpc.SendRequest) (*wavewalletrpc.SendResponse, error) {

	return &wavewalletrpc.SendResponse{}, nil
}

// InspectActivity returns a successful empty response after authorization.
func (*scopedMacaroonWalletServer) InspectActivity(context.Context,
	*wavewalletrpc.InspectActivityRequest) (
	*wavewalletrpc.InspectActivityResponse, error) {

	return &wavewalletrpc.InspectActivityResponse{}, nil
}

// Deposit returns a successful empty response if authorization lets it run.
func (*scopedMacaroonWalletServer) Deposit(context.Context,
	*wavewalletrpc.DepositRequest) (*wavewalletrpc.DepositResponse, error) {

	return &wavewalletrpc.DepositResponse{}, nil
}

// SweepWallet returns a successful empty response if authorization permits.
func (*scopedMacaroonWalletServer) SweepWallet(context.Context,
	*wavewalletrpc.SweepWalletRequest) (*wavewalletrpc.SweepWalletResponse,
	error) {

	return &wavewalletrpc.SweepWalletResponse{}, nil
}

// Exit returns a successful empty response if authorization permits.
func (*scopedMacaroonWalletServer) Exit(context.Context,
	*wavewalletrpc.ExitRequest) (*wavewalletrpc.ExitResponse, error) {

	return &wavewalletrpc.ExitResponse{}, nil
}

// TestScopedPaymentMacaroon verifies a credential baked by the running daemon
// can use only the exact payment workflow methods requested by the caller.
func TestScopedPaymentMacaroon(t *testing.T) {
	walletServer := &scopedMacaroonWalletServer{}
	fixture := newDirectedSendFixture(t, func(cfg *waved.Config) {
		cfg.RPC.NoTLS = false
		cfg.RPC.NoMacaroons = false
		securityDir := filepath.Join(
			cfg.DataDir, "data", cfg.Network,
		)
		cfg.RPC.TLSCertPath = filepath.Join(securityDir, "tls.cert")
		cfg.RPC.TLSKeyPath = filepath.Join(securityDir, "tls.key")
		cfg.RPC.MacaroonPath = filepath.Join(
			securityDir, "admin.macaroon",
		)
		cfg.RPCServiceRegistrars = append(
			cfg.RPCServiceRegistrars,
			func(_ context.Context, grpcServer *grpc.Server,
				_ *waved.RPCServer, _ *waved.Config) (func(),
				error) {

				wavewalletrpc.RegisterWalletServiceServer(
					grpcServer, walletServer,
				)
				registerInspection := wavewalletrpc.
					RegisterWalletInspectionServiceServer
				registerInspection(grpcServer, walletServer)

				return nil, nil
			},
		)
	})

	adminClient := waverpc.NewMacaroonServiceClient(fixture.conn)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()

	permissionResp, err := adminClient.ListPermissions(
		ctx, &waverpc.ListPermissionsRequest{},
	)
	require.NoError(t, err)

	allowedMethods := []string{
		wavewalletrpc.WalletService_Balance_FullMethodName,
		wavewalletrpc.WalletService_List_FullMethodName,
		wavewalletrpc.WalletService_PrepareSend_FullMethodName,
		wavewalletrpc.WalletService_Send_FullMethodName,
		wavewalletrpc.
			WalletInspectionService_InspectActivity_FullMethodName,
	}
	permissions := make(
		[]*waverpc.MacaroonPermission, 0, len(allowedMethods),
	)
	for _, fullMethod := range allowedMethods {
		require.Contains(
			t, permissionResp.GetMethodPermissions(), fullMethod,
		)
		permissions = append(permissions, &waverpc.MacaroonPermission{
			Entity: macaroons.PermissionEntityCustomURI,
			Action: fullMethod,
		})
	}

	bakeResp, err := adminClient.BakeMacaroon(
		ctx, &waverpc.BakeMacaroonRequest{
			Permissions: permissions,
		},
	)
	require.NoError(t, err)

	macBytes, err := hex.DecodeString(bakeResp.GetMacaroon())
	require.NoError(t, err)
	macaroonPath := filepath.Join(t.TempDir(), "payment.macaroon")
	require.NoError(t, os.WriteFile(macaroonPath, macBytes, 0o600))

	paymentConn, err := fixture.dial(macaroonPath)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, paymentConn.Close())
	})

	walletClient := wavewalletrpc.NewWalletServiceClient(paymentConn)
	inspectionClient :=
		wavewalletrpc.NewWalletInspectionServiceClient(paymentConn)

	_, err = walletClient.Balance(ctx, &wavewalletrpc.BalanceRequest{})
	require.NoError(t, err)
	_, err = walletClient.List(ctx, &wavewalletrpc.ListRequest{})
	require.NoError(t, err)
	_, err = walletClient.PrepareSend(
		ctx, &wavewalletrpc.PrepareSendRequest{},
	)
	require.NoError(t, err)
	_, err = walletClient.Send(ctx, &wavewalletrpc.SendRequest{})
	require.NoError(t, err)
	_, err = inspectionClient.InspectActivity(
		ctx, &wavewalletrpc.InspectActivityRequest{},
	)
	require.NoError(t, err)

	btcwalletClient := btcwalletrpc.NewWalletServiceClient(paymentConn)
	daemonClient := waverpc.NewDaemonServiceClient(paymentConn)
	restrictedMacaroonClient :=
		waverpc.NewMacaroonServiceClient(paymentConn)

	deniedCalls := map[string]func() error{
		"deposit": func() error {
			_, err := walletClient.Deposit(
				ctx, &wavewalletrpc.DepositRequest{},
			)

			return err
		},
		"sweep wallet": func() error {
			_, err := walletClient.SweepWallet(
				ctx, &wavewalletrpc.SweepWalletRequest{},
			)

			return err
		},
		"exit": func() error {
			_, err := walletClient.Exit(
				ctx, &wavewalletrpc.ExitRequest{},
			)

			return err
		},
		"wallet signing": func() error {
			_, err := btcwalletClient.SignTransaction(
				ctx, &btcwalletrpc.SignTransactionRequest{},
			)

			return err
		},
		"recovery": func() error {
			_, err := daemonClient.ArmVHTLCRecovery(
				ctx, &waverpc.ArmVHTLCRecoveryRequest{},
			)

			return err
		},
		"administration": func() error {
			_, err := restrictedMacaroonClient.BakeMacaroon(
				ctx, &waverpc.BakeMacaroonRequest{
					Permissions: permissions,
				},
			)

			return err
		},
	}

	for name, call := range deniedCalls {
		t.Run(name, func(t *testing.T) {
			err := call()
			require.Error(t, err)
			require.Contains(t, err.Error(), "permission denied")
		})
	}
}
