package waved

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

// testConfigWithInjectedRPCListener returns a config that is valid for
// Validate while relying on an injected RPC listener instead of a listen
// address string.
func testConfigWithInjectedRPCListener(listener net.Listener) *Config {
	cfg := DefaultConfig()
	cfg.Network = "regtest"
	// A syntactically valid mailbox host is still required by daemon
	// config validation even though the tests below only exercise the
	// local RPC server.
	cfg.Server.Host = "127.0.0.1:10010"
	cfg.Wallet.EsploraURL = "http://127.0.0.1:3000"
	cfg.RPC.ListenAddr = ""
	cfg.RPC.Listener = listener
	cfg.RPC.Gateway.Enabled = false
	cfg.RPC.NoTLS = true
	cfg.RPC.NoMacaroons = true

	return cfg
}

// TestConfigValidateAllowsInjectedRPCListener verifies embedders can skip
// ListenAddr entirely when they provide a pre-created listener in code.
func TestConfigValidateAllowsInjectedRPCListener(t *testing.T) {
	t.Parallel()

	listener := bufconn.Listen(1024)
	t.Cleanup(func() {
		require.NoError(t, listener.Close())
	})

	cfg := testConfigWithInjectedRPCListener(listener)

	require.NoError(t, cfg.Validate())
}

// TestDefaultConfigEnablesGateway ensures the daemon HTTP gateway is enabled
// on localhost by default.
func TestDefaultConfigEnablesGateway(t *testing.T) {
	t.Parallel()

	cfg := DefaultConfig()
	require.True(t, cfg.RPC.Gateway.Enabled)
	require.Equal(t, DefaultRPCGatewayHost, cfg.RPC.Gateway.ListenAddr)
}

// TestConfigValidateAllowsDisabledGateway verifies embedders can disable the
// HTTP gateway entirely.
func TestConfigValidateAllowsDisabledGateway(t *testing.T) {
	t.Parallel()

	cfg := DefaultConfig()
	cfg.Network = "regtest"
	cfg.Wallet.EsploraURL = "http://127.0.0.1:3000"
	cfg.RPC.Gateway.Enabled = false
	cfg.RPC.Gateway.ListenAddr = ""

	require.NoError(t, cfg.Validate())
}

// TestConfigValidateAllowsWildcardGatewayOrigin verifies public browser
// gateway deployments can opt into wildcard CORS.
func TestConfigValidateAllowsWildcardGatewayOrigin(t *testing.T) {
	t.Parallel()

	cfg := DefaultConfig()
	cfg.Network = "regtest"
	cfg.Wallet.EsploraURL = "http://127.0.0.1:3000"
	cfg.RPC.Gateway.AllowedOrigins = []string{
		"*",
	}

	require.NoError(t, cfg.Validate())
}

// TestConfigValidateRejectsEmptyGatewayOrigin keeps blank origin entries from
// silently weakening browser-gateway configuration.
func TestConfigValidateRejectsEmptyGatewayOrigin(t *testing.T) {
	t.Parallel()

	cfg := DefaultConfig()
	cfg.Network = "regtest"
	cfg.Wallet.EsploraURL = "http://127.0.0.1:3000"
	cfg.RPC.Gateway.AllowedOrigins = []string{
		" ",
	}

	err := cfg.Validate()
	require.ErrorContains(
		t, err, "rpc.gateway.allowedorigins must contain explicit "+
			"origins or '*'",
	)
}

// TestOpenRPCListenerUsesInjectedListener verifies the daemon reuses an
// injected listener verbatim so embedded runtimes can own the transport.
func TestOpenRPCListenerUsesInjectedListener(t *testing.T) {
	t.Parallel()

	listener := bufconn.Listen(1024)
	t.Cleanup(func() {
		require.NoError(t, listener.Close())
	})

	server := &Server{
		cfg: testConfigWithInjectedRPCListener(listener),
	}

	lis, err := server.openRPCListener()
	require.NoError(t, err)
	require.Same(t, listener, lis)
	require.Equal(t, listener.Addr(), server.RPCAddr())
}

// TestOpenRPCListenerBindsListenAddr verifies the standalone daemon path still
// binds a fresh TCP listener when no listener is injected.
func TestOpenRPCListenerBindsListenAddr(t *testing.T) {
	t.Parallel()

	cfg := DefaultConfig()
	cfg.RPC.ListenAddr = "127.0.0.1:0"

	server := &Server{cfg: cfg}

	lis, err := server.openRPCListener()
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, lis.Close())
	})

	tcpAddr, ok := lis.Addr().(*net.TCPAddr)
	require.True(t, ok)
	require.NotZero(t, tcpAddr.Port)
	require.Equal(t, lis.Addr(), server.RPCAddr())
}

// TestRunWithContextServesInjectedListener verifies the daemon can boot on an
// injected in-memory listener and serve a real gRPC request before the wallet
// has been initialized.
func TestRunWithContextServesInjectedListener(t *testing.T) {
	t.Parallel()

	listener := bufconn.Listen(1 << 20)
	cfg := testConfigWithInjectedRPCListener(listener)
	cfg.DataDir = t.TempDir()
	cfg.Wallet.Type = WalletTypeLwwallet
	cfg.Wallet.DisableGlobalLoggers = true

	server, err := NewServer(cfg)
	require.NoError(t, err)

	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errChan := make(chan error, 1)
	go func() {
		errChan <- server.RunWithContext(runCtx)
	}()

	require.Eventually(t, func() bool {
		return server.RPCAddr() != nil
	}, 30*time.Second, 20*time.Millisecond)

	connCtx, connCancel := context.WithTimeout(
		context.Background(), 5*time.Second,
	)
	defer connCancel()

	conn, err := grpc.NewClient(
		"passthrough:///bufnet",
		grpc.WithTransportCredentials(
			insecure.NewCredentials(),
		),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (
			net.Conn, error) {

			return listener.DialContext(ctx)
		}),
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, conn.Close())
	})

	conn.Connect()
	require.Eventually(t, func() bool {
		return conn.GetState() == connectivity.Ready
	}, 5*time.Second, 20*time.Millisecond)

	client := waverpc.NewDaemonServiceClient(conn)

	resp, err := client.GenSeed(connCtx, &waverpc.GenSeedRequest{})
	require.NoError(t, err)
	require.Len(t, resp.Mnemonic, 24)
	require.Equal(t, listener.Addr(), server.RPCAddr())

	cancel()
	require.NoError(t, <-errChan)
}
