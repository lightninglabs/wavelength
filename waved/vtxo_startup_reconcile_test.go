package waved

import (
	"context"
	"errors"
	"testing"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/chainsource"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// TestWalletBackendReadyDoesNotAdmitRPCs verifies backend synchronization is
// only the trigger for internal startup. Wallet RPCs remain unavailable until
// recovered VTXOs have crossed the explicit public-ready boundary.
func TestWalletBackendReadyDoesNotAdmitRPCs(t *testing.T) {
	t.Parallel()

	server := &Server{
		walletBackendReady: make(chan struct{}),
		walletReady:        make(chan struct{}),
	}
	rpcServer := &RPCServer{server: server}

	server.markWalletBackendReady()
	require.True(t, server.isWalletBackendReady())
	require.False(t, server.isWalletReady())
	require.Equal(t, WalletStateSyncing, server.WalletLifecycleState())
	require.Error(t, rpcServer.requireWalletReady())

	server.markWalletReady()
	require.True(t, server.isWalletReady())
	require.Equal(t, WalletStateReady, server.WalletLifecycleState())
	require.NoError(t, rpcServer.requireWalletReady())
}

// TestStartupReconcileUsesBestHeight verifies the daemon asks the chain source
// first and forwards that exact synchronized height into required VTXO
// classification.
func TestStartupReconcileUsesBestHeight(t *testing.T) {
	t.Parallel()

	system := actor.NewActorSystem()
	t.Cleanup(func() {
		require.NoError(t, system.Shutdown(context.Background()))
	})

	chainBehavior := actor.NewFunctionBehavior(
		func(_ context.Context,
			msg chainsource.ChainSourceMsg,
		) fn.Result[chainsource.ChainSourceResp] {

			require.IsType(t, &chainsource.BestHeightRequest{}, msg)

			return fn.Ok[chainsource.ChainSourceResp](
				&chainsource.BestHeightResponse{
					Height: 721,
				},
			)
		},
	)
	chainKey := actor.NewServiceKey[
		chainsource.ChainSourceMsg, chainsource.ChainSourceResp,
	](
		"startup-chain-source",
	)
	chainRef := actor.RegisterWithSystem(
		system, "startup-chain-source", chainKey, chainBehavior,
	)

	managerBehavior := actor.NewFunctionBehavior(
		func(_ context.Context,
			msg vtxo.ManagerMsg) fn.Result[vtxo.ManagerResp] {

			reconcile, ok := msg.(*vtxo.ReconcileExpiryRequest)
			require.True(
				t, ok, "unexpected manager request %T", msg,
			)
			require.Equal(t, int32(721), reconcile.Height)

			return fn.Ok[vtxo.ManagerResp](
				&vtxo.ReconcileExpiryResponse{
					Checked: 2,
					Expired: 1,
				},
			)
		},
	)
	managerKey := actor.NewServiceKey[vtxo.ManagerMsg, vtxo.ManagerResp](
		"startup-vtxo-manager",
	)
	managerRef := actor.RegisterWithSystem(
		system, "startup-vtxo-manager", managerKey, managerBehavior,
	)

	server := &Server{
		vtxoMgrRef: fn.Some(managerRef),
		log:        btclog.Disabled,
	}
	require.NoError(
		t,
		server.reconcileVTXOExpiryAtBestHeight(
			t.Context(), chainRef,
		),
	)
}

// TestStartupReconcileFailureBlocksReadiness verifies a failed local status
// write is surfaced to the startup caller instead of marking the daemon ready
// with stale spendable VTXOs.
func TestStartupReconcileFailureBlocksReadiness(t *testing.T) {
	t.Parallel()

	system := actor.NewActorSystem()
	t.Cleanup(func() {
		require.NoError(t, system.Shutdown(context.Background()))
	})

	chainBehavior := actor.NewFunctionBehavior(
		func(context.Context,
			chainsource.ChainSourceMsg,
		) fn.Result[chainsource.ChainSourceResp] {

			return fn.Ok[chainsource.ChainSourceResp](
				&chainsource.BestHeightResponse{
					Height: 800,
				},
			)
		},
	)
	chainKey := actor.NewServiceKey[
		chainsource.ChainSourceMsg, chainsource.ChainSourceResp,
	](
		"failing-startup-chain-source",
	)
	chainRef := actor.RegisterWithSystem(
		system, "failing-startup-chain-source", chainKey, chainBehavior,
	)

	managerBehavior := actor.NewFunctionBehavior(
		func(context.Context,
			vtxo.ManagerMsg) fn.Result[vtxo.ManagerResp] {

			return fn.Err[vtxo.ManagerResp](
				errors.New("status write"),
			)
		},
	)
	managerKey := actor.NewServiceKey[vtxo.ManagerMsg, vtxo.ManagerResp](
		"failing-startup-vtxo-manager",
	)
	managerRef := actor.RegisterWithSystem(
		system, "failing-startup-vtxo-manager", managerKey,
		managerBehavior,
	)

	server := &Server{
		vtxoMgrRef: fn.Some(managerRef),
		log:        btclog.Disabled,
	}
	err := server.reconcileVTXOExpiryAtBestHeight(
		t.Context(), chainRef,
	)
	require.ErrorContains(t, err, "status write")
}
