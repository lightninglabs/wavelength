package harness

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/lightninglabs/darepo"
	"github.com/lightninglabs/darepo-client/daemonrpc"
	clientdarepod "github.com/lightninglabs/darepo-client/darepod"
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

	// arkdLogFile is the operator log file kept open for the lifetime of
	// the in-process server.
	arkdLogFile *os.File

	// ArkAdminAddr is the host:port to reach arkd admin RPC.
	ArkAdminAddr string

	// ArkRPCAddr is the host:port to reach arkd client RPC.
	ArkRPCAddr string

	// ArkAdminClient is a connected arkd admin client.
	ArkAdminClient adminrpc.OperatorAdminClient

	// clientDaemons tracks any in-process darepod instances started through
	// this harness so Stop can shut them down before tearing down arkd/LND.
	clientDaemons map[string]*ClientDaemonHarness

	// clientDaemonsMu guards clientDaemons for concurrent test helper
	// access.
	clientDaemonsMu sync.Mutex
}

// NewArkHarness creates a new ArkHarness instance from the given options.
func NewArkHarness(t *testing.T, opts *ArkHarnessOptions) *ArkHarness {
	clientHarness := client_harness.NewHarness(t, opts.ClientOptions)

	return &ArkHarness{
		Harness:       clientHarness,
		skipArkd:      opts.SkipArkd,
		clientDaemons: make(map[string]*ClientDaemonHarness),
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
	// Stop any real client daemons first so their mailbox/runtime loops
	// shut down before we tear down arkd and the underlying chain
	// infrastructure.
	h.clientDaemonsMu.Lock()
	clientDaemons := make([]*ClientDaemonHarness, 0, len(h.clientDaemons))
	for _, daemon := range h.clientDaemons {
		clientDaemons = append(clientDaemons, daemon)
	}
	h.clientDaemons = make(map[string]*ClientDaemonHarness)
	h.clientDaemonsMu.Unlock()

	for _, daemon := range clientDaemons {
		daemon.Stop()
	}

	// Stop arkd first, if it was started.
	if !h.skipArkd {
		if h.arkdCancel != nil {
			h.arkdCancel()
			h.arkdWg.Wait()
		}

		if h.arkdAdminConn != nil {
			_ = h.arkdAdminConn.Close()
		}

		if h.arkdLogFile != nil {
			_ = h.arkdLogFile.Close()
			h.arkdLogFile = nil
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
	arkLogFile, err := os.OpenFile(
		arkLogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600,
	)
	require.NoError(h.T, err, "failed to open arkd log file")
	h.arkdLogFile = arkLogFile

	// Build config for arkd. Use port 0 to let the OS allocate free
	// ports, avoiding TOCTOU race conditions in parallel test execution.
	cfg := darepo.DefaultConfig()
	cfg.DataDir = h.arkDataDir
	cfg.Network = "regtest"
	cfg.DebugLevel = "trace"
	cfg.LogFilePath = arkLogPath
	cfg.LogWriter = io.MultiWriter(
		newPrefixedWriter(os.Stdout, "operator"), arkLogFile,
	)
	cfg.DB.Backend = "sqlite"
	cfg.DB.Sqlite.DatabaseFileName = filepath.Join(
		h.arkDataDir, "darepo.db",
	)
	cfg.AdminRPC.ListenAddr = "127.0.0.1:0"
	cfg.RPC.ListenAddr = "127.0.0.1:0"

	// Point arkd at the LND started by the client harness.
	// Derive credential paths from the harness artifacts directory
	// since the client harness fields are unexported.
	lndDataDir := filepath.Join(h.Harness.BaseDir(), "lnd")
	cfg.Lnd.Host = fmt.Sprintf(
		"127.0.0.1:%s", h.Harness.LNDGRPCPort,
	)
	cfg.Lnd.TLSPath = filepath.Join(lndDataDir, "tls.cert")
	cfg.Lnd.MacaroonPath = filepath.Join(
		lndDataDir, "data", "chain", "bitcoin", "regtest",
		"admin.macaroon",
	)

	// Create a cancelable context for arkd.
	arkdCtx, arkdCancel := context.WithCancel(context.Background())
	h.arkdCancel = arkdCancel

	// Create arkd server.
	server, err := darepo.NewServer(cfg)
	require.NoError(h.T, err, "failed to create arkd server")
	h.arkdServer = server

	// Run arkd in a goroutine.
	h.arkdWg.Add(1)
	go func() {
		defer h.arkdWg.Done()

		if err := server.RunWithContext(arkdCtx); err != nil {
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
			h.T.Context(), defaultSmallTimeout,
		)
		defer cancel()

		conn, err := grpc.Dial(
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

// ClientDaemonHarness manages one real in-process darepod instance plus its
// backing LND node and connected daemon RPC client.
type ClientDaemonHarness struct {
	T *testing.T

	Name string

	// DataDir is the daemon's root data directory under the harness
	// artifacts tree.
	DataDir string

	// RPCAddr is the daemon gRPC address used by external clients.
	RPCAddr string

	// LocalMailboxID is the darepod mailbox ID used for inbound pulls.
	LocalMailboxID string

	// RemoteMailboxID is the per-client server mailbox ID this daemon talks
	// to.
	RemoteMailboxID string

	// LND is the dedicated backing LND instance for this daemon.
	LND *client_harness.LndInstance

	// RPCConn is the connected daemon gRPC client transport.
	RPCConn *grpc.ClientConn

	// RPCClient is the typed daemon gRPC client used by integration tests.
	RPCClient daemonrpc.DaemonServiceClient

	server *clientdarepod.Server

	cancel context.CancelFunc
	wg     sync.WaitGroup

	runErr chan error

	logFile *os.File
}

// StartClientDaemon starts a real in-process darepod backed by a dedicated LND
// instance and connects a daemon RPC client to it.
func (h *ArkHarness) StartClientDaemon(name string) *ClientDaemonHarness {
	h.T.Helper()

	require.NotEmpty(h.T, h.ArkRPCAddr, "arkd must be started first")
	require.NotEmpty(h.T, name, "client daemon name is required")

	h.clientDaemonsMu.Lock()
	if _, ok := h.clientDaemons[name]; ok {
		h.clientDaemonsMu.Unlock()
		h.T.Fatalf("client daemon %q already exists", name)
	}
	h.clientDaemonsMu.Unlock()

	lnd := h.StartAdditionalLND(name)
	dataDir := filepath.Join(h.BaseDir(), "client-daemons", name)
	localMailboxID := fmt.Sprintf("client-%s", name)
	remoteMailboxID := fmt.Sprintf("server-for-%s", name)

	daemon := h.launchClientDaemon(
		name, lnd, dataDir, localMailboxID, remoteMailboxID,
	)

	h.clientDaemonsMu.Lock()
	h.clientDaemons[name] = daemon
	h.clientDaemonsMu.Unlock()

	return daemon
}

// RestartClientDaemon restarts an existing in-process darepod instance while
// reusing its data directory, mailbox IDs, and backing LND node.
func (h *ArkHarness) RestartClientDaemon(name string) *ClientDaemonHarness {
	h.T.Helper()

	h.clientDaemonsMu.Lock()
	oldDaemon, ok := h.clientDaemons[name]
	if !ok {
		h.clientDaemonsMu.Unlock()
		h.T.Fatalf("client daemon %q not found", name)
	}
	delete(h.clientDaemons, name)
	h.clientDaemonsMu.Unlock()

	oldRPCAddr := oldDaemon.RPCAddr
	oldDaemon.Stop()

	daemon := h.launchClientDaemon(
		name, oldDaemon.LND, oldDaemon.DataDir,
		oldDaemon.LocalMailboxID, oldDaemon.RemoteMailboxID,
	)

	h.clientDaemonsMu.Lock()
	h.clientDaemons[name] = daemon
	h.clientDaemonsMu.Unlock()

	h.T.Logf("restarted client daemon %q: old_rpc=%s new_rpc=%s",
		name, oldRPCAddr, daemon.RPCAddr)

	return daemon
}

func (h *ArkHarness) launchClientDaemon(name string,
	lnd *client_harness.LndInstance, dataDir, localMailboxID,
	remoteMailboxID string) *ClientDaemonHarness {

	h.T.Helper()

	require.NotNil(h.T, lnd, "backing LND instance is required")
	require.NotEmpty(h.T, dataDir, "client daemon data dir is required")
	require.NotEmpty(h.T, localMailboxID, "local mailbox ID is required")
	require.NotEmpty(h.T, remoteMailboxID, "remote mailbox ID is required")

	require.NoError(h.T, os.MkdirAll(dataDir, 0o755))
	logPath := filepath.Join(dataDir, "darepod.log")
	logFile, err := os.OpenFile(
		logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600,
	)
	require.NoError(h.T, err, "failed to open client daemon log file")

	cfg := clientdarepod.DefaultConfig()
	cfg.DataDir = dataDir
	cfg.Network = "regtest"
	cfg.DebugLevel = "trace"
	cfg.LogWriter = io.MultiWriter(
		newPrefixedWriter(os.Stdout, name), logFile,
	)
	cfg.Wallet.Type = clientdarepod.WalletTypeLnd
	cfg.Lnd.Host = net.JoinHostPort("127.0.0.1", lnd.GRPCPort)
	cfg.Lnd.TLSPath = lnd.TLSCert
	cfg.Lnd.MacaroonPath = lnd.Macaroon
	cfg.Server.Host = h.ArkRPCAddr
	cfg.Server.Insecure = true
	cfg.Server.LocalMailboxID = localMailboxID
	cfg.Server.RemoteMailboxID = remoteMailboxID
	cfg.RPC.ListenAddr = "127.0.0.1:0"

	require.NoError(h.T, cfg.Validate())

	server, err := clientdarepod.NewServer(cfg)
	require.NoError(h.T, err, "failed to create client daemon %q", name)

	ctx, cancel := context.WithCancel(context.Background())
	daemon := &ClientDaemonHarness{
		T:               h.T,
		Name:            name,
		DataDir:         dataDir,
		LocalMailboxID:  localMailboxID,
		RemoteMailboxID: remoteMailboxID,
		LND:             lnd,
		server:          server,
		cancel:          cancel,
		runErr:          make(chan error, 1),
		logFile:         logFile,
	}

	daemon.wg.Add(1)
	go func() {
		defer daemon.wg.Done()
		daemon.runErr <- server.RunWithContext(ctx)
	}()

	require.Eventually(h.T, func() bool {
		addr := server.RPCAddr()
		if addr == nil {
			return false
		}

		daemon.RPCAddr = addr.String()

		return true
	}, defaultTimeout, pollInterval,
		"client daemon %q RPC address never became available", name)

	daemon.waitForReady()

	return daemon
}

// GetClientDaemon returns a previously started client daemon by name.
func (h *ArkHarness) GetClientDaemon(name string) *ClientDaemonHarness {
	h.T.Helper()

	h.clientDaemonsMu.Lock()
	defer h.clientDaemonsMu.Unlock()

	daemon, ok := h.clientDaemons[name]
	if !ok {
		h.T.Fatalf("client daemon %q not found", name)
	}

	return daemon
}

// Stop gracefully shuts down the daemon and closes the connected RPC client.
func (d *ClientDaemonHarness) Stop() {
	if d == nil {
		return
	}

	if d.cancel != nil {
		d.cancel()
	}

	d.wg.Wait()

	if d.RPCConn != nil {
		_ = d.RPCConn.Close()
	}

	if d.logFile != nil {
		_ = d.logFile.Close()
		d.logFile = nil
	}
}

// waitForReady polls the daemon RPC until GetInfo succeeds, proving the daemon
// is reachable through its real public gRPC surface.
func (d *ClientDaemonHarness) waitForReady() {
	d.T.Helper()

	require.Eventually(d.T, func() bool {
		select {
		case err := <-d.runErr:
			if err != nil {
				d.T.Fatalf(
					"client daemon %q exited during "+
						"startup: %v",
					d.Name, err,
				)
			}
			d.T.Fatalf(
				"client daemon %q exited during startup",
				d.Name,
			)
		default:
		}

		ctx, cancel := context.WithTimeout(
			d.T.Context(), defaultSmallTimeout,
		)
		defer cancel()

		conn, client, err := d.getDaemonServiceClient()
		if err != nil {
			return false
		}

		resp, err := client.GetInfo(ctx, &daemonrpc.GetInfoRequest{})
		if err != nil {
			_ = conn.Close()

			return false
		}

		d.RPCConn = conn
		d.RPCClient = client
		d.T.Logf("client daemon %s ready: network=%s wallet_type=%s",
			d.Name, resp.Network, resp.WalletType)

		return true
	}, defaultTimeout, pollInterval,
		fmt.Sprintf("client daemon %q RPC not ready", d.Name))
}

// getDaemonServiceClient creates a fresh insecure daemon RPC client for this
// in-process test daemon.
func (d *ClientDaemonHarness) getDaemonServiceClient() (*grpc.ClientConn,
	daemonrpc.DaemonServiceClient, error) {

	conn, err := grpc.Dial(
		d.RPCAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, nil, err
	}

	return conn, daemonrpc.NewDaemonServiceClient(conn), nil
}
