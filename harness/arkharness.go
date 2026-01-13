package harness

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/lightninglabs/darepo"
	client_harness "github.com/lightninglabs/darepo-client/harness"
	"github.com/lightninglabs/darepo/adminrpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const (
	// defaultSmallTimeout is the default timeout for quick RPC calls.
	defaultSmallTimeout = 5 * time.Second

	// defaultTimeout is the default timeout for operations that may take
	// longer.
	defaultTimeout = 30 * time.Second

	// pollInterval is the interval at which to poll for conditions.
	pollInterval = 200 * time.Millisecond
)

// ArkHarnessOptions configures the ArkHarness behavior.
type ArkHarnessOptions struct {
	// ClientOptions are the options for the underlying client harness.
	ClientOptions *client_harness.Options

	// SkipArkd when true prevents arkd from being started. This is useful
	// for in-process e2e tests where the server actors are run directly as
	// goroutines rather than through the arkd binary.
	SkipArkd bool
}

// ArkHarness extends the client harness with an in-process arkd server and
// admin RPC connection. It provides all the infrastructure (bitcoind, lnd,
// tapd, electrs) via the embedded client harness, and adds the arkd server
// on top.
type ArkHarness struct {
	*client_harness.Harness

	// skipArkd when true means arkd was not started. This is set based on
	// ArkHarnessOptions.SkipArkd.
	skipArkd bool

	// arkdServer is the in-process arkd server.
	arkdServer *darepo.Server

	// arkdCancel controls the arkd server lifecycle.
	arkdCancel context.CancelFunc

	// arkdWg is used to wait for the arkd server to shut down.
	arkdWg sync.WaitGroup

	// arkdAdminConn is the gRPC connection to arkd admin RPC.
	arkdAdminConn *grpc.ClientConn

	// arkDataDir is the ark data dir (for logs and db etc).
	arkDataDir string

	// ArkAdminAddr is the host:port to reach arkd admin RPC.
	ArkAdminAddr string

	// ArkRPCAddr is the host:port to reach arkd client RPC.
	ArkRPCAddr string

	// ArkAdminClient is a connected arkd admin client.
	ArkAdminClient adminrpc.OperatorAdminClient
}

// NewArkHarness creates a new ArkHarness instance from the given options.
func NewArkHarness(t *testing.T, opts *ArkHarnessOptions) *ArkHarness {
	clientHarness := client_harness.NewHarness(t, opts.ClientOptions)

	return &ArkHarness{
		Harness:  clientHarness,
		skipArkd: opts.SkipArkd,
	}
}

// Start starts the harness infrastructure and optionally the in-process arkd
// server (unless SkipArkd was set in options).
func (h *ArkHarness) Start() {
	// Start the client harness first (bitcoind, lnd, tapd, electrs).
	h.Harness.Start()

	// Now start arkd on top, unless we're skipping it for in-process tests.
	if !h.skipArkd {
		h.startArkd()
	}
}

// Stop stops the arkd server (if started) and then the underlying
// infrastructure.
func (h *ArkHarness) Stop() {
	// Stop arkd first, if it was started.
	if !h.skipArkd {
		if h.arkdCancel != nil {
			h.arkdCancel()
			h.arkdWg.Wait()
		}

		if h.arkdAdminConn != nil {
			_ = h.arkdAdminConn.Close()
		}
	}

	// Stop the client harness.
	h.Harness.Stop()
}

// startArkd starts the in-process arkd server and connects to its admin RPC.
func (h *ArkHarness) startArkd() {
	// Prepare arkd data directory.
	h.arkDataDir = filepath.Join(h.Harness.BaseDir(), "arkd")
	require.NoError(h.T, os.MkdirAll(h.arkDataDir, 0755))

	arkLogPath := filepath.Join(h.arkDataDir, "arkd.log")

	// Build config for arkd. Use port 0 to let the OS allocate free
	// ports, avoiding TOCTOU race conditions in parallel test execution.
	cfg := darepo.DefaultConfig()
	cfg.DataDir = h.arkDataDir
	cfg.Network = "regtest"
	cfg.LogLevel = "trace"
	cfg.LogFilePath = arkLogPath
	cfg.DB.Backend = "sqlite"
	cfg.DB.Sqlite.DatabaseFileName = filepath.Join(
		h.arkDataDir, "darepo.db",
	)
	cfg.AdminRPC.RPCListen = "127.0.0.1:0"
	cfg.RPC.RPCListen = "127.0.0.1:0"

	// Create a cancelable context for arkd.
	arkdCtx, arkdCancel := context.WithCancel(context.Background())
	h.arkdCancel = arkdCancel

	// Create arkd server.
	server, err := darepo.NewServer(arkdCtx, &cfg)
	require.NoError(h.T, err, "failed to create arkd server")
	h.arkdServer = server

	// Run arkd in a goroutine.
	h.arkdWg.Add(1)
	go func() {
		defer h.arkdWg.Done()

		if err := server.RunUntilShutdown(arkdCtx); err != nil {
			h.T.Logf("arkd exited with error: %v", err)
		}
	}()

	// Wait for the server to start and retrieve the actual bound
	// addresses. Poll until both addresses are available.
	require.Eventually(h.T, func() bool {
		// Check if server failed to start.
		select {
		case <-arkdCtx.Done():
			h.T.Fatal("arkd context cancelled during startup")

			return false

		default:
		}

		adminAddr := server.AdminRPCAddr()
		rpcAddr := server.RPCAddr()

		if adminAddr == nil || rpcAddr == nil {
			return false
		}

		h.ArkAdminAddr = adminAddr.String()
		h.ArkRPCAddr = rpcAddr.String()

		return true
	}, defaultTimeout, pollInterval, "arkd server addresses not available")

	h.T.Logf("arkd listening on AdminRPC=%s, ClientRPC=%s",
		h.ArkAdminAddr, h.ArkRPCAddr)

	// Wait for arkd admin RPC to be ready by connecting and calling Info.
	require.Eventually(h.T, func() bool {
		ctx, cancel := context.WithTimeout(
			context.Background(), defaultSmallTimeout,
		)
		defer cancel()

		conn, err := grpc.NewClient(
			h.ArkAdminAddr, grpc.WithTransportCredentials(
				insecure.NewCredentials(),
			),
		)
		if err != nil {
			return false
		}

		client := adminrpc.NewOperatorAdminClient(conn)
		_, err = client.Info(ctx, &adminrpc.InfoRequest{})
		if err != nil {
			_ = conn.Close()

			return false
		}

		// Success - save the connection and client.
		h.arkdAdminConn = conn
		h.ArkAdminClient = client

		return true
	}, defaultTimeout, pollInterval, "arkd admin RPC not ready")

	// Log success.
	ctx, cancel := context.WithTimeout(
		context.Background(), defaultSmallTimeout,
	)
	defer cancel()

	resp, err := h.ArkAdminClient.Info(ctx, &adminrpc.InfoRequest{})
	require.NoError(h.T, err, "failed to get arkd info")
	h.T.Logf("arkd started successfully, version=%s", resp.Version)
}
