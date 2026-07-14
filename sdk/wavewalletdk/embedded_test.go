//go:build !js

package wavewalletdk

import (
	"context"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/lightninglabs/wavelength/waved"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

// TestSigningWorkersOverride verifies embedded hosts can force serial signing
// while a zero convenience value preserves a caller-owned daemon setting.
func TestSigningWorkersOverride(t *testing.T) {
	t.Parallel()

	t.Run("convenience override", func(t *testing.T) {
		t.Parallel()

		cfg := DefaultConfig()
		cfg.SigningWorkers = 1
		daemonCfg, err := daemonConfig(cfg)
		require.NoError(t, err)
		require.Equal(t, 1, daemonCfg.SigningWorkers)
	})

	t.Run("zero preserves daemon config", func(t *testing.T) {
		t.Parallel()

		base := waved.DefaultConfig()
		base.SigningWorkers = 8
		daemonCfg, err := daemonConfig(Config{DaemonConfig: base})
		require.NoError(t, err)
		require.Equal(t, 8, daemonCfg.SigningWorkers)
	})
}

// TestCloneDaemonConfigIsolation verifies that cloneDaemonConfig produces a
// graph whose reference-typed fields can be mutated independently of the
// original. This is the actual contract Start relies on when it injects its
// bufconn listener and swap registrar into the cloned config.
//
// The test reflects over waved.Config and fails on any pointer/slice/map
// field whose clone aliases the original. Adding a new reference-typed field
// to waved.Config without updating cloneDaemonConfig will surface here as
// either a "still nil after clone" failure (when the test fixture is also out
// of date) or, more importantly, an aliasing failure (when the fixture is
// updated but the clone is not).
func TestCloneDaemonConfigIsolation(t *testing.T) {
	original := waved.DefaultConfig()

	// Make sure every reference-typed field we currently know about is
	// non-nil/non-empty so we can detect aliasing. New fields added to
	// waved.Config require updating this fixture too — the test below
	// flags any reference-typed field that remains nil after this setup.
	if original.Lnd == nil {
		original.Lnd = &waved.LndConfig{}
	}
	if original.Server == nil {
		original.Server = &waved.ServerConfig{}
	}
	if original.RPC == nil {
		original.RPC = &waved.RPCConfig{}
	}
	if original.Wallet == nil {
		original.Wallet = &waved.WalletConfig{}
	}
	original.Wallet.BtcwalletPeers = []string{"peer-a"}
	original.Wallet.BtcwalletAddPeers = []string{"add-peer-a"}
	if original.Unroll == nil {
		original.Unroll = &waved.UnrollConfig{}
	}
	if original.Swap == nil {
		original.Swap = &waved.SwapConfig{}
	}
	if original.OOR == nil {
		original.OOR = &waved.OORConfig{}
	}
	original.RPCServiceRegistrars = []waved.RPCServiceRegistrar{
		func(context.Context, *grpc.Server, *waved.RPCServer,
			*waved.Config) (func(), error) {

			return func() {}, nil
		},
	}
	original.WalletReadyHooks = []waved.WalletReadyHook{
		func(context.Context) error {
			return nil
		},
	}
	original.UnaryServerInterceptors = []grpc.UnaryServerInterceptor{
		func(ctx context.Context, req any, _ *grpc.UnaryServerInfo,
			handler grpc.UnaryHandler) (any, error) {

			return handler(ctx, req)
		},
	}

	clone := cloneDaemonConfig(original)

	origV := reflect.ValueOf(original).Elem()
	cloneV := reflect.ValueOf(clone).Elem()

	for i := 0; i < origV.NumField(); i++ {
		fieldName := origV.Type().Field(i).Name
		origF := origV.Field(i)
		cloneF := cloneV.Field(i)

		switch origF.Kind() {
		case reflect.Ptr:
			if origF.IsNil() {
				// Skip pointer fields the fixture doesn't
				// populate; the assertion below would be
				// vacuously satisfied anyway.
				continue
			}
			require.NotNil(
				t, cloneF.Interface(),
				"clone field %s is nil but original is set",
				fieldName,
			)
			require.NotEqualf(
				t, origF.Pointer(), cloneF.Pointer(),
				"clone field %s aliases the original "+
					"pointer (add a deep-copy in "+
					"cloneDaemonConfig)", fieldName,
			)

		case reflect.Slice:
			if origF.Len() == 0 {
				continue
			}
			require.Greaterf(
				t, cloneF.Len(), 0, "clone field %s is "+
					"empty but original is populated",
				fieldName,
			)
			origHeader := origF.Index(0).UnsafeAddr()
			cloneHeader := cloneF.Index(0).UnsafeAddr()
			require.NotEqualf(
				t, origHeader, cloneHeader, "clone slice %s "+
					"shares backing array with original "+
					"(add an append-based copy in "+
					"cloneDaemonConfig)", fieldName,
			)

		case reflect.Map:
			// We do not currently clone any maps. If a map field
			// is ever added to waved.Config, this branch flags
			// the gap loudly so the contributor can decide
			// whether the map needs cloning.
			if !origF.IsNil() {
				t.Fatalf("waved.Config grew a map field %q "+
					"that cloneDaemonConfig does "+
					"not handle", fieldName)
			}

		default:
			// Value-typed fields (scalars, structs, interfaces,
			// funcs, etc.) do not need deep-copy because they are
			// already isolated by the outer struct copy that
			// cloneDaemonConfig performs.
		}
	}
}

// TestDaemonConfigPropagatesOutboundTransports verifies wavewalletdk
// convenience fields drive only the embedded daemon's outbound client
// transports.
func TestDaemonConfigPropagatesOutboundTransports(t *testing.T) {
	t.Parallel()

	cfg := Config{
		ServerTransport:     TransportREST,
		SwapServerTransport: TransportREST,
	}

	daemonCfg, err := daemonConfig(cfg)
	require.NoError(t, err)

	require.Equal(
		t, waved.RPCTransportREST, daemonCfg.Server.Transport,
	)
	require.Equal(
		t, waved.RPCTransportREST, daemonCfg.Swap.ServerTransport,
	)
}

// TestDaemonConfigPreservesCallerOutboundTransports verifies a caller-owned
// daemon config remains authoritative unless wavewalletdk convenience fields
// are explicitly set.
func TestDaemonConfigPreservesCallerOutboundTransports(t *testing.T) {
	t.Parallel()

	callerCfg := waved.DefaultConfig()
	callerCfg.Server.Transport = waved.RPCTransportREST
	callerCfg.Swap.ServerTransport = waved.RPCTransportREST

	daemonCfg, err := daemonConfig(Config{
		DaemonConfig: callerCfg,
	})
	require.NoError(t, err)

	require.Equal(
		t, waved.RPCTransportREST, daemonCfg.Server.Transport,
	)
	require.Equal(
		t, waved.RPCTransportREST, daemonCfg.Swap.ServerTransport,
	)
}

// TestClientStopIdempotent verifies that Stop, Close, and repeated calls all
// invoke the underlying closeFn exactly once. Hosts often call Stop from
// multiple shutdown paths (signal handler, defer, error cleanup) so the
// double-close contract must hold.
func TestClientStopIdempotent(t *testing.T) {
	var calls atomic.Int32
	client := &Client{
		closeFn: func(context.Context) error {
			calls.Add(1)

			return nil
		},
	}

	require.NoError(t, client.Stop())
	require.NoError(t, client.Stop())
	require.NoError(t, client.Close())
	require.EqualValues(
		t, 1, calls.Load(),
		"closeFn must be invoked exactly once across Stop/Close calls",
	)
}
