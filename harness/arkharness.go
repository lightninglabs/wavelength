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
	mailboxpb "github.com/lightninglabs/darepo-client/mailbox/pb"
	"github.com/lightninglabs/darepo/adminrpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

const (
	// defaultSmallTimeout is the default timeout for quick RPC calls.
	defaultSmallTimeout = 5 * time.Second

	// defaultTimeout is the default timeout for operations that may take
	// longer.
	defaultTimeout = 30 * time.Second

	// pollInterval is the interval at which to poll for conditions.
	pollInterval = 200 * time.Millisecond

	// clientWalletBackendEnv controls which wallet backend the daemon
	// integration harness uses for in-process client daemons.
	clientWalletBackendEnv = "ARK_ITEST_CLIENT_WALLET"

	// ClientWalletBackendLND runs client daemons backed by per-daemon lnd
	// instances.
	ClientWalletBackendLND = clientdarepod.WalletTypeLnd

	// ClientWalletBackendLWWallet runs client daemons using the in-process
	// lightweight wallet backend.
	ClientWalletBackendLWWallet = clientdarepod.WalletTypeLwwallet

	// ClientWalletBackendBtcwallet runs client daemons using the
	// in-process neutrino-backed btcwallet backend.
	ClientWalletBackendBtcwallet = clientdarepod.WalletTypeBtcwallet

	// defaultLWWalletPassword is the deterministic test password used by
	// the harness when creating and unlocking lwwallet- and
	// btcwallet-backed daemons.
	defaultLWWalletPassword = "itest-wallet-password"
)

// ClientDaemonName is the logical identifier used to index client daemons
// started by the harness.
type ClientDaemonName string

// ClientDaemonSet tracks started client daemons by name.
type ClientDaemonSet map[ClientDaemonName]*ClientDaemonHarness

// ClientLogWriterFactory returns the stdout sink to use for a named client
// daemon's logs.
type ClientLogWriterFactory func(name string) io.Writer

type mailboxEdgeFactory = clientdarepod.MailboxEdgeFactory
type mailboxClient = mailboxpb.MailboxServiceClient

// ArkHarnessOptions configures the ArkHarness behavior.
type ArkHarnessOptions struct {
	// ClientOptions are the options for the underlying client harness.
	ClientOptions *client_harness.Options

	// SkipArkd when true prevents arkd from being started. This is useful
	// for in-process e2e tests where the server actors are run directly as
	// goroutines rather than through the arkd binary.
	SkipArkd bool

	// ClientDaemonWalletType selects the wallet backend for in-process
	// client daemons. Valid values: "lnd", "lwwallet", "btcwallet".
	// If empty, the value is read from ARK_ITEST_CLIENT_WALLET and
	// defaults to "lnd".
	ClientDaemonWalletType string

	// OperatorLogWriter is the stdout sink used for operator daemon logs.
	// When nil, logs are prefixed with [operator] and written to stdout.
	OperatorLogWriter io.Writer

	// ClientLogWriterFactory returns the stdout sink used for a named
	// client daemon. When nil, logs are prefixed by daemon name and
	// written to stdout.
	ClientLogWriterFactory ClientLogWriterFactory
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

	// clientDaemonWalletType selects which wallet backend newly launched
	// in-process client daemons use.
	clientDaemonWalletType string

	// operatorLogWriter is the stdout sink used for operator daemon logs.
	operatorLogWriter io.Writer

	// clientLogWriterFactory resolves stdout sinks for client daemon logs.
	clientLogWriterFactory ClientLogWriterFactory

	// clientDaemons tracks any in-process darepod instances started through
	// this harness so Stop can shut them down before tearing down arkd/LND.
	clientDaemons ClientDaemonSet

	// clientDaemonsMu guards clientDaemons for concurrent test helper
	// access.
	clientDaemonsMu sync.Mutex

	// clientMailboxEdges keeps per-daemon mailbox wrappers so tests can
	// pause selected outbound durable transport messages across daemon
	// restarts.
	clientMailboxEdges map[ClientDaemonName]*ControlledMailboxClient
}

// NewArkHarness creates a new ArkHarness instance from the given options.
func NewArkHarness(t *testing.T, opts *ArkHarnessOptions) *ArkHarness {
	if opts == nil {
		opts = &ArkHarnessOptions{}
	}

	if opts.ClientOptions == nil {
		defaultOpts := client_harness.DefaultOptions()
		opts.ClientOptions = &defaultOpts
	}

	walletType, err := resolveClientDaemonWalletType(
		opts.ClientDaemonWalletType,
	)
	require.NoError(t, err)

	operatorLogWriter := opts.OperatorLogWriter
	if operatorLogWriter == nil {
		operatorLogWriter = newPrefixedWriter(
			os.Stdout, "operator",
		)
	}

	clientLogWriterFactory := opts.ClientLogWriterFactory
	if clientLogWriterFactory == nil {
		clientLogWriterFactory = func(name string) io.Writer {
			return newPrefixedWriter(os.Stdout, name)
		}
	}

	clientHarness := client_harness.NewHarness(t, opts.ClientOptions)

	return &ArkHarness{
		Harness:                clientHarness,
		skipArkd:               opts.SkipArkd,
		clientDaemonWalletType: walletType,
		operatorLogWriter:      operatorLogWriter,
		clientLogWriterFactory: clientLogWriterFactory,
		clientDaemons:          make(ClientDaemonSet),
		clientMailboxEdges: make(
			map[ClientDaemonName]*ControlledMailboxClient,
		),
	}
}

func resolveClientDaemonWalletType(requestedType string) (string, error) {
	backend := requestedType
	if backend == "" {
		backend = os.Getenv(clientWalletBackendEnv)
	}
	if backend == "" {
		return ClientWalletBackendLND, nil
	}

	switch backend {
	case ClientWalletBackendLND, ClientWalletBackendLWWallet,
		ClientWalletBackendBtcwallet:

		return backend, nil

	default:
		return "", fmt.Errorf(
			"invalid client daemon wallet backend %q",
			backend,
		)
	}
}

// ClientWalletBackend returns the wallet backend type used by client
// daemons in this harness.
func (h *ArkHarness) ClientWalletBackend() string {
	return h.clientDaemonWalletType
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
	h.clientDaemons = make(ClientDaemonSet)
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
		h.operatorLogWriter, arkLogFile,
	)
	cfg.DB.Backend = "sqlite"
	cfg.DB.Sqlite.DatabaseFileName = filepath.Join(
		h.arkDataDir, "darepo.db",
	)
	cfg.AdminRPC.ListenAddr = "127.0.0.1:0"
	cfg.RPC.ListenAddr = "127.0.0.1:0"
	cfg.Metrics = nil

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

// ClientDaemonHarness manages one real in-process darepod instance and its
// connected daemon RPC client.
type ClientDaemonHarness struct {
	T *testing.T

	Name string

	// DataDir is the daemon's root data directory under the harness
	// artifacts tree.
	DataDir string

	// RPCAddr is the daemon gRPC address used by external clients.
	RPCAddr string

	// LND is the dedicated backing LND instance for this daemon when
	// running with the lnd wallet backend. It is nil for lwwallet
	// daemons.
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

// StartClientDaemon starts a real in-process darepod and connects a daemon RPC
// client to it.
func (h *ArkHarness) StartClientDaemon(name string) *ClientDaemonHarness {
	h.T.Helper()

	require.NotEmpty(h.T, h.ArkRPCAddr, "arkd must be started first")
	require.NotEmpty(h.T, name, "client daemon name is required")
	daemonName := ClientDaemonName(name)

	h.clientDaemonsMu.Lock()
	if _, ok := h.clientDaemons[daemonName]; ok {
		h.clientDaemonsMu.Unlock()
		h.T.Fatalf("client daemon %q already exists", name)
	}
	h.clientDaemonsMu.Unlock()

	var lnd *client_harness.LndInstance
	if h.clientDaemonWalletType == ClientWalletBackendLND {
		lnd = h.StartAdditionalLND(name)
	}

	dataDir := filepath.Join(h.BaseDir(), "client-daemons", name)
	daemon := h.launchClientDaemon(name, lnd, dataDir)

	h.clientDaemonsMu.Lock()
	h.clientDaemons[daemonName] = daemon
	h.clientDaemonsMu.Unlock()

	return daemon
}

// RestartClientDaemon restarts an existing in-process darepod instance while
// reusing its data directory, mailbox IDs, and backing wallet resources.
func (h *ArkHarness) RestartClientDaemon(name string) *ClientDaemonHarness {
	h.T.Helper()

	daemonName := ClientDaemonName(name)
	h.clientDaemonsMu.Lock()
	oldDaemon, ok := h.clientDaemons[daemonName]
	if !ok {
		h.clientDaemonsMu.Unlock()
		h.T.Fatalf("client daemon %q not found", name)
	}
	delete(h.clientDaemons, daemonName)
	h.clientDaemonsMu.Unlock()

	oldRPCAddr := oldDaemon.RPCAddr
	oldDaemon.Stop()

	daemon := h.launchClientDaemon(
		name, oldDaemon.LND, oldDaemon.DataDir,
	)

	h.clientDaemonsMu.Lock()
	h.clientDaemons[daemonName] = daemon
	h.clientDaemonsMu.Unlock()

	h.T.Logf("restarted client daemon %q: old_rpc=%s new_rpc=%s",
		name, oldRPCAddr, daemon.RPCAddr)

	return daemon
}

// ClientMailbox returns the controlled mailbox wrapper for a started client
// daemon so integration tests can pause specific outbound transport messages.
func (h *ArkHarness) ClientMailbox(name string) *ControlledMailboxClient {
	h.T.Helper()

	h.clientDaemonsMu.Lock()
	defer h.clientDaemonsMu.Unlock()

	edge, ok := h.clientMailboxEdges[ClientDaemonName(name)]
	if !ok {
		h.T.Fatalf("client mailbox edge %q not found", name)
	}

	return edge
}

func (h *ArkHarness) launchClientDaemon(name string,
	lnd *client_harness.LndInstance,
	dataDir string) *ClientDaemonHarness {

	h.T.Helper()

	require.NotEmpty(h.T, dataDir, "client daemon data dir is required")

	require.NoError(h.T, os.MkdirAll(dataDir, 0o755))
	logPath := filepath.Join(dataDir, "darepod.log")
	logFile, err := os.OpenFile(
		logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600,
	)
	require.NoError(h.T, err, "failed to open client daemon log file")

	cfg := clientdarepod.DefaultConfig()
	cfg.DataDir = dataDir
	cfg.Network = "regtest"
	// Use trace for daemon subsystems but cap BTCW at debug —
	// neutrino's internal trace logging is extremely verbose
	// and floods test output.
	cfg.DebugLevel = "trace,BTCW=debug"
	cfg.LogWriter = io.MultiWriter(
		h.clientLogWriterFactory(name), logFile,
	)

	switch h.clientDaemonWalletType {
	case ClientWalletBackendLND:
		require.NotNil(h.T, lnd, "backing LND instance is required")
		cfg.Wallet.Type = clientdarepod.WalletTypeLnd
		cfg.Lnd.Host = net.JoinHostPort("127.0.0.1", lnd.GRPCPort)
		cfg.Lnd.TLSPath = lnd.TLSCert
		cfg.Lnd.MacaroonPath = lnd.Macaroon

	case ClientWalletBackendLWWallet:
		cfg.Wallet.Type = clientdarepod.WalletTypeLwwallet
		cfg.Wallet.EsploraURL = h.Harness.EsploraURL

	case ClientWalletBackendBtcwallet:
		cfg.Wallet.Type = clientdarepod.WalletTypeBtcwallet
		cfg.Wallet.BtcwalletPeers = []string{
			h.Harness.BitcoindP2P,
		}
		cfg.Wallet.FeeURL = h.Harness.EsploraURL +
			"/api/v1/fees/recommended"
		cfg.Wallet.PersistFilters = true

	default:
		h.T.Fatalf("unsupported client daemon wallet backend: %s",
			h.clientDaemonWalletType)
	}

	cfg.Server.Host = h.ArkRPCAddr
	cfg.Server.Insecure = true
	cfg.RPC.ListenAddr = "127.0.0.1:0"

	mailboxEdge := h.clientMailboxEdge(ClientDaemonName(name))
	cfg.MailboxEdgeFactory = newEdgeFactory(mailboxEdge)

	require.NoError(h.T, cfg.Validate())

	server, err := clientdarepod.NewServer(cfg)
	require.NoError(h.T, err, "failed to create client daemon %q", name)

	ctx, cancel := context.WithCancel(context.Background())
	daemon := &ClientDaemonHarness{
		T:       h.T,
		Name:    name,
		DataDir: dataDir,
		LND:     lnd,
		server:  server,
		cancel:  cancel,
		runErr:  make(chan error, 1),
		logFile: logFile,
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
	daemon.ensureWalletReady(h.clientDaemonWalletType)

	// Wait for the full daemon stack (mailbox transport + actors)
	// to finish initialization. For LND this is immediate since
	// everything runs synchronously. For lwwallet/btcwallet the
	// deferred goroutine needs time after wallet unlock.
	select {
	case <-server.DaemonReady():
	case <-time.After(defaultTimeout):
		h.T.Fatalf(
			"client daemon %q never became fully ready",
			name,
		)
	}

	return daemon
}

func newEdgeFactory(edge *ControlledMailboxClient) mailboxEdgeFactory {
	return func(conn grpc.ClientConnInterface) mailboxClient {
		edge.SetInner(mailboxpb.NewMailboxServiceClient(conn))

		return edge
	}
}

func (h *ArkHarness) clientMailboxEdge(
	name ClientDaemonName) *ControlledMailboxClient {

	h.clientDaemonsMu.Lock()
	defer h.clientDaemonsMu.Unlock()

	if edge, ok := h.clientMailboxEdges[name]; ok {
		return edge
	}

	edge := NewControlledMailboxClient()
	h.clientMailboxEdges[name] = edge

	return edge
}

// GetClientDaemon returns a previously started client daemon by name.
func (h *ArkHarness) GetClientDaemon(name string) *ClientDaemonHarness {
	h.T.Helper()

	h.clientDaemonsMu.Lock()
	defer h.clientDaemonsMu.Unlock()

	daemon, ok := h.clientDaemons[ClientDaemonName(name)]
	if !ok {
		h.T.Fatalf("client daemon %q not found", name)
	}

	return daemon
}

// TriggerRoundRegistration advances the daemon's queued round intents by
// injecting RegistrationRequested into the underlying round actor.
func (d *ClientDaemonHarness) TriggerRoundRegistration() {
	d.T.Helper()

	ctx, cancel := context.WithTimeout(d.T.Context(), defaultSmallTimeout)
	defer cancel()

	require.NotNil(d.T, d.server, "client daemon server is not initialized")
	require.NoError(d.T, d.server.TriggerRoundRegistration(ctx))
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

func (d *ClientDaemonHarness) ensureWalletReady(walletBackend string) {
	d.T.Helper()

	ctx, cancel := context.WithTimeout(
		d.T.Context(), defaultSmallTimeout,
	)
	defer cancel()

	info, err := d.RPCClient.GetInfo(ctx, &daemonrpc.GetInfoRequest{})
	require.NoError(d.T, err, "GetInfo RPC failed")
	if info.WalletReady {
		return
	}

	require.Contains(
		d.T,
		[]string{
			ClientWalletBackendLWWallet,
			ClientWalletBackendBtcwallet,
		},
		walletBackend,
		"wallet must already be ready for backend %q", walletBackend,
	)

	if err := d.initOrUnlockLWWallet(); err != nil {
		d.T.Fatalf(
			"failed to initialize wallet-backed daemon %q: %v",
			d.Name, err,
		)
	}

	// Neutrino-backed wallets need extra time for initial header
	// and compact block filter sync before marking wallet ready,
	// but keep the bound close to the unlock budget so restart
	// regressions surface promptly.
	walletReadyTimeout := defaultTimeout
	if walletBackend == ClientWalletBackendBtcwallet {
		walletReadyTimeout = 90 * time.Second
	}

	require.Eventually(d.T, func() bool {
		waitCtx, waitCancel := context.WithTimeout(
			d.T.Context(), defaultSmallTimeout,
		)
		defer waitCancel()

		updatedInfo, getInfoErr := d.RPCClient.GetInfo(
			waitCtx, &daemonrpc.GetInfoRequest{},
		)
		if getInfoErr != nil {
			return false
		}

		return updatedInfo.WalletReady
	}, walletReadyTimeout, pollInterval,
		"client daemon %q wallet never became ready", d.Name)
}

func (d *ClientDaemonHarness) initOrUnlockLWWallet() error {
	// Self-managed wallet unlock can take materially longer than a
	// quick RPC budget during restart. Both lwwallet and btcwallet
	// must reopen btcwallet state, restart their chain backend, and
	// derive the identity key before UnlockWallet returns. Use a
	// generous but still bounded timeout so restart tests fail on
	// real recovery bugs rather than a 5s harness deadline.
	timeout := 90 * time.Second

	ctx, cancel := context.WithTimeout(d.T.Context(), timeout)
	defer cancel()

	seedResp, err := d.RPCClient.GenSeed(
		ctx, &daemonrpc.GenSeedRequest{},
	)
	if err == nil {
		_, initErr := d.RPCClient.InitWallet(
			ctx, &daemonrpc.InitWalletRequest{
				Mnemonic:       seedResp.Mnemonic,
				WalletPassword: []byte(defaultLWWalletPassword),
			},
		)
		if initErr != nil {
			return fmt.Errorf("InitWallet RPC failed: %w", initErr)
		}

		return nil
	}

	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.FailedPrecondition {
		return fmt.Errorf("GenSeed RPC failed: %w", err)
	}

	_, unlockErr := d.RPCClient.UnlockWallet(
		ctx, &daemonrpc.UnlockWalletRequest{
			WalletPassword: []byte(defaultLWWalletPassword),
		},
	)
	if unlockErr != nil {
		return fmt.Errorf("UnlockWallet RPC failed: %w", unlockErr)
	}

	return nil
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
