package darepod

import (
	"testing"

	btcrpcserver "github.com/btcsuite/btcwallet/rpc/rpcserver"
	btcwalletrpc "github.com/btcsuite/btcwallet/rpc/walletrpc"
	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"gopkg.in/macaroon-bakery.v2/bakery"
)

// TestDarepodRPCPermissionsMapsReadMethods verifies read-only methods require
// the read permission.
func TestDarepodRPCPermissionsMapsReadMethods(t *testing.T) {
	t.Parallel()

	for _, fullMethod := range []string{
		daemonrpc.DaemonService_GetInfo_FullMethodName,
		daemonrpc.DaemonService_ListTransactions_FullMethodName,
		daemonrpc.DaemonService_WatchRounds_FullMethodName,
		daemonrpc.DaemonService_EstimateFee_FullMethodName,
		daemonrpc.DaemonService_GetBalance_FullMethodName,
	} {
		ops, ok := darepodRPCPermissions[fullMethod]
		require.True(t, ok, fullMethod)
		require.Equal(
			t, []string{"read"}, opActions(
				ops,
			),
			fullMethod,
		)
	}
}

// TestDarepodRPCPermissionsMapsMutatingMethods verifies mutating methods
// require the write permission even when their names could look query-shaped.
func TestDarepodRPCPermissionsMapsMutatingMethods(t *testing.T) {
	t.Parallel()

	fullDaemonMethod := func(method string) string {
		return "/" + daemonrpc.DaemonService_ServiceDesc.ServiceName +
			"/" + method
	}

	for _, fullMethod := range []string{
		daemonrpc.DaemonService_GenSeed_FullMethodName,
		daemonrpc.DaemonService_UnlockWallet_FullMethodName,
		daemonrpc.DaemonService_ReceiveAuthKey_FullMethodName,
		daemonrpc.DaemonService_RefreshVTXOs_FullMethodName,
		daemonrpc.DaemonService_SignReceiveAuthMessage_FullMethodName,
		fullDaemonMethod("SubmitForfeitParticipantSignatures"),
	} {
		ops, ok := darepodRPCPermissions[fullMethod]
		require.True(t, ok, fullMethod)
		require.Equal(
			t, []string{"write"}, opActions(
				ops,
			),
			fullMethod,
		)
	}
}

// TestDarepodRPCPermissionsCoversBtcwallet verifies btcwallet's public RPC
// surface is fully covered by the daemon's fail-closed permission map.
func TestDarepodRPCPermissionsCoversBtcwallet(t *testing.T) {
	t.Parallel()

	grpcServer := grpc.NewServer()
	t.Cleanup(grpcServer.Stop)

	btcrpcserver.StartVersionService(grpcServer)
	btcwalletrpc.RegisterWalletServiceServer(
		grpcServer, &btcwalletrpc.UnimplementedWalletServiceServer{},
	)

	permissions, err := registeredRPCPermissions(grpcServer)
	require.NoError(t, err)

	for _, fullMethod := range []string{
		"/walletrpc.WalletService/TransactionNotifications",
		"/walletrpc.WalletService/SpentnessNotifications",
		"/walletrpc.WalletService/AccountNotifications",
	} {
		ops, ok := permissions[fullMethod]
		require.True(t, ok, fullMethod)
		require.Equal(t, []string{"read"}, opActions(ops), fullMethod)
	}
}

// opActions extracts macaroon action strings for assertions.
func opActions(ops []bakery.Op) []string {
	actions := make([]string, 0, len(ops))
	for _, op := range ops {
		actions = append(actions, op.Action)
	}

	return actions
}
