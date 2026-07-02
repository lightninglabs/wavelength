package darepod

import (
	"testing"

	btcrpcserver "github.com/btcsuite/btcwallet/rpc/rpcserver"
	btcwalletrpc "github.com/btcsuite/btcwallet/rpc/walletrpc"
	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/lightninglabs/darepo-client/rpc/swapclientrpc"
	"github.com/lightninglabs/darepo-client/rpc/walletdkrpc"
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

// TestDarepodRPCPermissionsUseGranularEntities verifies the permission map is
// sliced into multiple logical entities rather than a single all-or-nothing
// entity, and that every method names a known entity.
func TestDarepodRPCPermissionsUseGranularEntities(t *testing.T) {
	t.Parallel()

	known := make(map[string]struct{}, len(darepodEntities))
	for _, entity := range darepodEntities {
		known[entity] = struct{}{}
	}

	seen := make(map[string]struct{})
	for fullMethod, ops := range darepodRPCPermissions {
		require.NotEmpty(t, ops, fullMethod)
		for _, op := range ops {
			_, ok := known[op.Entity]
			require.True(
				t, ok, "%s: unknown entity %q", fullMethod,
				op.Entity,
			)
			seen[op.Entity] = struct{}{}
		}
	}

	// The whole point of the taxonomy: more than one entity is in use, so
	// least-privilege macaroons are possible.
	require.Greater(t, len(seen), 1)
}

// TestDarepodRPCPermissionsEntityMapping pins a representative method from each
// domain to its expected entity so a careless remap is caught.
func TestDarepodRPCPermissionsEntityMapping(t *testing.T) {
	t.Parallel()

	fullDaemonMethod := func(method string) string {
		return "/" + daemonrpc.DaemonService_ServiceDesc.ServiceName +
			"/" + method
	}

	cases := map[string]string{
		fullDaemonMethod("GetInfo"):          entityInfo,
		fullDaemonMethod("ListVTXOs"):        entityVTXO,
		fullDaemonMethod("SendVTXO"):         entityVTXO,
		fullDaemonMethod("NewAddress"):       entityAddress,
		fullDaemonMethod("Board"):            entityOnChain,
		fullDaemonMethod("SendOnChain"):      entityOnChain,
		fullDaemonMethod("SendOOR"):          entityOOR,
		fullDaemonMethod("JoinNextRound"):    entityRound,
		fullDaemonMethod("EstimateFee"):      entityFees,
		fullDaemonMethod("ArmVHTLCRecovery"): entityRecovery,
		fullDaemonMethod("ListTransactions"): entityActivity,
	}

	for fullMethod, entity := range cases {
		ops, ok := darepodRPCPermissions[fullMethod]
		require.True(t, ok, fullMethod)
		require.Len(t, ops, 1, fullMethod)
		require.Equal(t, entity, ops[0].Entity, fullMethod)
	}
}

// TestDarepodReadOnlyPermissions verifies the read-only permission set is one
// read op per entity, authorizes every read method, and authorizes no mutating
// method.
func TestDarepodReadOnlyPermissions(t *testing.T) {
	t.Parallel()

	readOnly := darepodReadOnlyPermissions()

	// The set must be exactly one read op per entity.
	require.Len(t, readOnly, len(darepodEntities))
	granted := make(map[bakery.Op]struct{}, len(readOnly))
	for _, op := range readOnly {
		require.Equal(t, "read", op.Action, op.Entity)
		granted[op] = struct{}{}
	}
	require.Len(t, granted, len(darepodEntities))

	// Every read method's ops must be granted; no write method's ops may be
	// granted. This is what makes the token strictly read-only.
	for fullMethod, ops := range darepodRPCPermissions {
		allRead := true
		for _, op := range ops {
			if op.Action != "read" {
				allRead = false
			}
		}

		for _, op := range ops {
			_, ok := granted[op]
			if allRead {
				require.True(
					t, ok, "%s: %v not granted", fullMethod,
					op,
				)
			} else if op.Action != "read" {
				require.False(
					t, ok, "%s: %v granted to read-only "+
						"token", fullMethod, op,
				)
			}
		}
	}
}

// TestDarepodRPCPermissionsCoverDaemonServices registers every service the
// swapruntime/walletdkrpc daemon serves and asserts the permission map covers
// all their methods, via the exact check the startup validator runs. Without
// this, an RPC added without a grant — as happened with the credit RPCs
// (CreateCredit/RedeemCredit/ListCredits) — only fails when the daemon refuses
// to boot, not in CI.
func TestDarepodRPCPermissionsCoverDaemonServices(t *testing.T) {
	t.Parallel()

	grpcServer := grpc.NewServer()
	t.Cleanup(grpcServer.Stop)

	daemonrpc.RegisterDaemonServiceServer(
		grpcServer, &daemonrpc.UnimplementedDaemonServiceServer{},
	)
	swapclientrpc.RegisterSwapClientServiceServer(
		grpcServer,
		&swapclientrpc.UnimplementedSwapClientServiceServer{},
	)
	walletdkrpc.RegisterWalletServiceServer(
		grpcServer, &walletdkrpc.UnimplementedWalletServiceServer{},
	)
	walletdkrpc.RegisterWalletInspectionServiceServer(
		grpcServer,
		&walletdkrpc.UnimplementedWalletInspectionServiceServer{},
	)

	_, err := registeredRPCPermissions(grpcServer)
	require.NoError(
		t, err, "every registered method needs a macaroon permission",
	)
}

// opActions extracts macaroon action strings for assertions.
func opActions(ops []bakery.Op) []string {
	actions := make([]string, 0, len(ops))
	for _, op := range ops {
		actions = append(actions, op.Action)
	}

	return actions
}
