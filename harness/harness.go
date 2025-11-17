// Package harness provides a small, programmatic integration-test runner that
// starts a regtest bitcoind and an lnd node in Docker, boots arkd in-process,
// and offers helpers for mining and funding. It also manages per-run artifacts
// (data directories and logs), supports parallel test execution through
// dynamic ports and per-run Docker networks, and guarantees clean teardown.
package harness

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/rpcclient"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/taproot-assets/taprpc"
	lnrpc "github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/macaroons"
	"github.com/ory/dockertest/v3"
	"github.com/ory/dockertest/v3/docker"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"gopkg.in/macaroon.v2"
)

const (
	// numInitialBlocks is the default number of blocks to pre-mine on
	// startup to ensure coinbase maturity (100 blocks) plus a 6-block
	// buffer for spending coinbase outputs in tests with some headroom.
	numInitialBlocks = 106

	// defaultTimeout is the default timeout for various operations
	// including RPC calls, container startup, and network operations.
	defaultTimeout = 30 * time.Second

	// pollInterval is the interval for polling in require.Eventually
	// calls. Set to 200ms to balance responsiveness with CPU usage during
	// test execution. This is used for most polling operations including
	// balance checks, peer connections, and chain sync.
	pollInterval = 200 * time.Millisecond

	// networkPrefix is the prefix for private Docker networks created
	// for each harness instance to ensure isolation between test runs.
	networkPrefix = "ark-itest-"

	// bitcoindRPCUser is the RPC username for bitcoind in regtest mode.
	bitcoindRPCUser = "admin1"

	// bitcoindRPCPass is the RPC password for bitcoind in regtest mode.
	bitcoindRPCPass = "123"

	// maxNetworkNameRetries is the number of times to retry creating a
	// unique network name on collision before giving up. Set to 5 to
	// handle rare cases of concurrent test execution creating networks
	// with colliding random suffixes.
	maxNetworkNameRetries = 5

	// blockTimeCushion adds margin beyond block time to account for clock
	// skew and processing delays in time-dependent operations. Set to 1
	// second to ensure reliable timing in tests that depend on block
	// timestamps.
	blockTimeCushion = 1000 * time.Millisecond
)

var (
	harnessLogStdOut = flag.Bool(
		"harness.logstdout", false,
		"if true, harness will log to stdout in addition to file",
	)

	harnessPostgres = flag.Bool(
		"harness.postgres", false,
		"if true, use PostgreSQL instead of SQLite for arkd",
	)

	artifactsBaseDirFlag = flag.String(
		"artifacts_base_dir", "",
		"Directory where the harness stores artifacts.",
	)

	// harnessHTTPClient is a dedicated HTTP client for harness HTTP
	// communication (bitcoind, electrs, etc.) with proper timeouts and
	// connection limits to prevent resource exhaustion during concurrent
	// test execution.
	harnessHTTPClient = &http.Client{
		Timeout: defaultTimeout,
		Transport: &http.Transport{
			MaxIdleConns:        10,
			MaxIdleConnsPerHost: 2,
			IdleConnTimeout:     30 * time.Second,
		},
	}
)

// Harness spins up an ARK test environment using bitcoind and LND in Docker
// and arkd in-process.
type Harness struct {
	T *testing.T

	// opts are the options used to start the harness.
	opts *Options

	// stopOnce ensures Stop is only executed once even if called multiple
	// times (for example via t.Cleanup and signal handler).
	stopOnce sync.Once

	// sigCh receives OS signals to trigger cleanup when tests are aborted
	// with Ctrl+C or similar.
	sigCh chan os.Signal

	// pool is the docker test pool to manage containers.
	pool *dockertest.Pool

	// network is a private docker network for the containers.
	network *dockertest.Network

	// bitcoind is the bitcoind container.
	bitcoind *dockertest.Resource

	// lnd is the lnd container.
	lnd *dockertest.Resource

	// tapd is the tapd container.
	tapd *dockertest.Resource

	// electrs is the electrs indexer container (Esplora HTTP API).
	electrs *dockertest.Resource

	// postgres is the postgres container (optional, only if
	// harness.postgres flag is set).
	postgres *dockertest.Resource

	// PostgresHost is the host:port for postgres connection.
	PostgresHost string

	// bitcoindName is the canonical name LND uses to reach bitcoind in the
	// same network.
	bitcoindName string

	// artifactsDir is the base dir for all artifacts (logs, data dirs).
	artifactsDir string

	// group is the optional group name for all resources (containers,
	// network, dirs).
	group string

	// bitcoinDataDir is the bitcoind data dir (for blocks, chainstate,
	// wallet).
	bitcoinDataDir string

	// lndDataDir is the lnd data dir (for tls.cert and admin.macaroon).
	lndDataDir string

	// lndTLSCert is the path to the TLS cert for LND.
	lndTLSCert string

	// lndMacaroon is the path to the admin macaroon for LND.
	lndMacaroon string

	// tapdDataDir is the tapd data dir.
	tapdDataDir string

	// tapdTLSCert is the path to the TLS cert for tapd.
	tapdTLSCert string

	// tapdMacaroon is the path to the admin macaroon for tapd.
	tapdMacaroon string

	// harnessLogFile is the file harness logs are written to.
	harnessLogFile *os.File

	// BitcoindRPC is the host:port of bitcoind RPC (18443).
	BitcoindRPC string

	// BitcoindZMQBlock is the host:port of bitcoind ZMQ for raw blocks
	// (28332).
	BitcoindZMQBlock string

	// BitcoindZMQTx is the host:port of bitcoind ZMQ for raw txs (28333).
	BitcoindZMQTx string

	// LNDGRPCPort is the host port mapped to lnd gRPC (10009).
	LNDGRPCPort string

	// LNDRestPort is the host port mapped to lnd REST (8080).
	LNDRestPort string

	// TapdGRPCPort is the host port mapped to tapd gRPC (10029).
	TapdGRPCPort string

	// TapdRestPort is the host port mapped to tapd REST (8089).
	TapdRestPort string

	// LND is the lndclient instance connected to the running LND.
	LND *lndclient.LndServices

	// extraLNDs holds additional LND instances spawned for tests.
	extraLNDs map[string]*LndInstance

	// EsploraURL is the base URL of the local electrs HTTP server.
	EsploraURL string
}

// LndInstance represents a running LND node spawned by the harness.
type LndInstance struct {
	Name          string
	Resource      *dockertest.Resource
	DataDir       string
	TLSCert       string
	Macaroon      string
	GRPCPort      string
	RESTPort      string
	Client        *lndclient.LndServices
	ContainerName string
}

// Options configures how the Harness is started, isolated and how artifacts
// and logs are handled.
type Options struct {
	// BitcoindImage is the docker image:tag to use for bitcoind.
	BitcoindImage string

	// LNDImage is the docker image:tag to use for lnd.
	LNDImage string

	// LNDBuildPath is optional: build LND image from local path instead of
	// pulling tag. Leave empty to skip build and pull image instead.
	LNDBuildPath string

	// TapdImage is the docker image:tag to use for tapd.
	TapdImage string

	// ArtifactsBaseDir is the base directory to create store artifacts in.
	ArtifactsBaseDir string

	// GroupName is an optional name to group all resources (containers,
	// network, dirs) under. If empty, a random suffix is used.
	GroupName string

	// HarnessLogStdOut if true, also prints harness logs to stdout in
	// addition to the harness log file.
	HarnessLogStdOut bool

	// ArkdLogStdOut if true, also prints arkd logs to stdout in
	// addition to the arkd log file.
	ArkdLogStdOut bool

	// StartTapd if true, starts a tapd instance along with the harness.
	// Default is false to speed up tests that don't need tapd.
	StartTapd bool

	// AlwaysKeepArtifacts if true, keeps artifacts dir even on successful
	// runs. By default, artifacts are kept.
	AlwaysKeepArtifacts bool
}

// DefaultOptions returns sensible defaults for running the harness locally.
func DefaultOptions() Options {
	artifactsBaseDir := "test-artifacts"
	if artifactsBaseDirFlag != nil && *artifactsBaseDirFlag != "" {
		artifactsBaseDir = *artifactsBaseDirFlag
	}

	return Options{
		BitcoindImage:       "lightninglabs/bitcoin-core:29",
		LNDImage:            "lightninglabs/lnd:v0.19.3-beta",
		TapdImage:           "lightninglabs/taproot-assets:v0.7.0-rc1",
		ArtifactsBaseDir:    artifactsBaseDir,
		HarnessLogStdOut:    *harnessLogStdOut,
		AlwaysKeepArtifacts: true,
	}
}

func (o *Options) validate(t *testing.T) {
	t.Helper()

	require.NotEmpty(t, o.ArtifactsBaseDir, "ArtifactsBaseDir must be "+
		"set")
}

// NewHarness creates a new Harness instance. If opts is nil the defaults from
// DefaultOptions() are used. The returned instance is not started yet, call
// Start() to launch the environment.
func NewHarness(t *testing.T, opts *Options) *Harness {
	t.Helper()

	if opts == nil {
		d := DefaultOptions()
		opts = &d
	}

	opts.validate(t)

	return &Harness{
		T:         t,
		opts:      opts,
		extraLNDs: make(map[string]*LndInstance),
	}
}

// shortPath returns the last n path elements (e.g., "pkg/file.go").
func shortPath(full string, n int) string {
	parts := []string{}
	for range n {
		base := filepath.Base(full)
		parts = append([]string{base}, parts...)
		full = filepath.Dir(full)
		if full == "." || full == "/" {
			break
		}
	}

	if len(parts) == 0 {
		return filepath.Base(full)
	}

	return filepath.Join(parts...)
}

// caller returns file:line for the frame `skip` up the stack.
func caller(skip int) (string, int) {
	// runtime.Caller(0) => this function
	// runtime.Caller(1) => the function that called caller()
	// ...
	_, file, line, ok := runtime.Caller(skip)
	if !ok {
		return "?:?", 0
	}

	return shortPath(file, 2), line
}

// logWithCaller centralizes timestamp + caller resolution. baseSkip accounts
// for caller() and logWithCaller() frames.
func (h *Harness) logWithCaller(additionalSkip int, msg string) {
	const baseSkip = 2
	file, line := caller(baseSkip + additionalSkip)

	const tsLayout = "2006-01-02 15:04:05.000"
	ts := time.Now().Format(tsLayout)

	logLine := fmt.Sprintf("%s [%s:%d] %s\n", ts, file, line, msg)
	if h.harnessLogFile != nil {
		var err error
		_, err = h.harnessLogFile.WriteString(logLine)
		require.NoError(h.T, err, "failed to write harness log")
	}

	if h.opts.HarnessLogStdOut {
		// Intentionally using Print as we want to ensure that logLine
		// is printed as-is without extra formatting.
		fmt.Print(logLine)
	}
}

// Log centralizes harness logging by printing timestamped messages with
// caller information to both file and optionally stdout, enabling easier
// debugging of test execution flow and post-mortem analysis when tests fail.
func (h *Harness) Log(args ...any) {
	h.logWithCaller(1, fmt.Sprint(args...))
}

// Logf centralizes harness logging by formatting and printing timestamped
// messages with caller information to both file and optionally stdout, using
// printf-style formatting for structured output during test execution.
func (h *Harness) Logf(format string, args ...any) {
	h.logWithCaller(1, fmt.Sprintf(format, args...))
}

// BaseDir returns the base directory where all artifacts (data dirs, logs) are
// stored.
func (h *Harness) BaseDir() string {
	return h.artifactsDir
}

// Start launches bitcoind and lnd containers, initializes lnd, and boots arkd
// in-process.
func (h *Harness) Start() {
	h.T.Helper()

	h.setupArtifactsAndLogging()
	h.setupDockerEnvironment()
	h.createDataDirectories()
	h.startInfrastructure()
	h.startLightningNetwork()
	h.setupSignalHandlers()
}

// setupArtifactsAndLogging creates the artifacts directory and initializes
// logging to both file and stdout.
func (h *Harness) setupArtifactsAndLogging() {
	// Use a group name for all resources if provided, otherwise random.
	// This allows grouping resources for easier inspection and cleanup.
	if h.opts.GroupName != "" {
		h.group = h.opts.GroupName
	} else {
		h.group = randSuffix()
	}

	require.NoError(h.T, os.MkdirAll(h.opts.ArtifactsBaseDir, 0o755))

	// Use human-readable timestamp format (YYYYMMDDhhmmss) for easier
	// navigation and identification of test artifacts.
	timestamp := time.Now().Format("20060102150405")
	h.artifactsDir = filepath.Join(
		h.opts.ArtifactsBaseDir, h.group, timestamp,
	)
	require.NoError(h.T, os.MkdirAll(h.artifactsDir, 0o755))

	// Set up harness logging with btclog to both stdout and file.
	harnessLogPath := filepath.Join(h.artifactsDir, "harness.log")
	var err error
	h.harnessLogFile, err = os.OpenFile(
		harnessLogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644,
	)
	require.NoError(h.T, err, "failed to open harness log file")

	h.Logf("Starting harness, artifacts dir: %v", h.artifactsDir)
}

// setupDockerEnvironment initializes the Docker pool, prunes stale networks,
// and creates an isolated network for this test run.
func (h *Harness) setupDockerEnvironment() {
	var err error
	h.pool, err = dockertest.NewPool("")
	require.NoError(h.T, err, "failed to init docker pool")

	h.pruneStaleHarnessNetworks()

	// Per-run isolation.
	h.Log("Creating docker network...")
	h.network, err = h.createNetworkUnique()
	require.NoError(h.T, err, "failed to create network")
	h.Logf("Docker network created: %s (id=%s)", h.network.Network.Name,
		h.network.Network.ID)
}

// createDataDirectories creates the necessary data directories for bitcoind,
// lnd, and tapd.
func (h *Harness) createDataDirectories() {
	h.bitcoinDataDir = filepath.Join(h.artifactsDir, "bitcoind")
	h.lndDataDir = filepath.Join(h.artifactsDir, "lnd")
	h.tapdDataDir = filepath.Join(h.artifactsDir, "tapd")

	require.NoError(h.T, os.MkdirAll(h.bitcoinDataDir, 0o755))
	require.NoError(h.T, os.MkdirAll(h.lndDataDir, 0o755))
	require.NoError(h.T, os.MkdirAll(h.tapdDataDir, 0o755))
}

// startInfrastructure starts the core infrastructure containers: bitcoind,
// electrs, and optionally postgres. Bitcoind and postgres start concurrently
// for better performance.
func (h *Harness) startInfrastructure() {
	// Start bitcoind and postgres concurrently (they are independent).
	// This saves time since postgres startup can overlap with bitcoind
	// initialization and block mining.
	var wg sync.WaitGroup
	if *harnessPostgres {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.Log("Starting postgres...")
			h.startPostgres()
		}()
	}

	h.Log("Starting bitcoind...")
	h.startBitcoind()

	h.Logf("Pre-mining %d regtest blocks...", numInitialBlocks)
	h.Generate(numInitialBlocks)

	// Start electrs (Esplora HTTP) after pre-mining to speed up initial
	// index.
	h.Log("Starting electrs (Esplora HTTP)...")
	h.startElectrs()

	// Wait for postgres to finish starting if it was requested.
	wg.Wait()

	h.Logf("Esplora URL available at: %s", h.EsploraURL)
}

// startLightningNetwork starts the primary LND node, initializes its wallet,
// and optionally starts tapd.
func (h *Harness) startLightningNetwork() {
	h.Log("Starting lnd...")
	primaryLND := h.startLND()

	h.Log("Initializing LND wallet if needed...")
	h.initAndWaitLND(primaryLND)

	if h.opts.StartTapd {
		h.Log("Starting tapd...")
		h.startTapd()
	} else {
		h.Log("Skipping tapd startup (StartTapd=false)")
	}
}

// setupSignalHandlers arranges for cleanup on Ctrl+C/termination as a safety
// net in case test cleanup doesn't run. This prevents orphaned containers.
func (h *Harness) setupSignalHandlers() {
	// The signal handler is safe to call concurrently with normal cleanup
	// because Stop() uses sync.Once internally.
	h.sigCh = make(chan os.Signal, 1)
	signal.Notify(
		h.sigCh, os.Interrupt, syscall.SIGINT, syscall.SIGTERM,
		syscall.SIGQUIT,
	)

	go func() {
		_, ok := <-h.sigCh
		if !ok {
			return
		}
		h.Log("signal received, stopping harness...")
		h.Stop()
	}()
}

// Stop tears down ark server and docker resources.
func (h *Harness) Stop() {
	h.stopOnce.Do(func() {
		h.disableSignalHandlers()
		h.Log("Stopping harness...")
		h.killContainers()
		h.saveLogsOnFailure()
		h.purgeDockerResources()
		h.Log("Harness stopped")
		h.cleanupArtifacts()
	})
}

// disableSignalHandlers stops receiving signals to avoid re-entry and closes
// the signal channel.
func (h *Harness) disableSignalHandlers() {
	if h.sigCh != nil {
		signal.Stop(h.sigCh)
		close(h.sigCh)
	}
}

// killContainers forcefully stops all running containers. This is faster than
// purging and allows us to save logs before removal.
func (h *Harness) killContainers() {
	h.Logf("Stopping docker containers...")

	// Kill in reverse startup order: electrs, postgres, tapd, lnd(s),
	// bitcoind. This ensures dependent services stop before their
	// dependencies.
	h.killContainer(h.electrs, "electrs")
	h.killContainer(h.postgres, "postgres")
	h.killContainer(h.tapd, "tapd")
	h.killContainer(h.lnd, "lnd")

	// Kill any additional LND instances.
	for name, inst := range h.extraLNDs {
		if inst != nil && inst.Resource != nil {
			h.killContainer(inst.Resource, name)
		}
	}

	h.killContainer(h.bitcoind, "bitcoind")
}

// killContainer kills a single container by ID, logging any errors.
func (h *Harness) killContainer(
	res *dockertest.Resource, name string) {

	if res == nil {
		return
	}

	err := h.pool.Client.KillContainer(docker.KillContainerOptions{
		ID: res.Container.ID,
	})
	if err != nil {
		h.Logf("failed to kill %s: %v", name, err)
	} else {
		h.Logf("%s killed", name)
	}
}

// saveLogsOnFailure saves container logs to the artifacts directory if the
// test failed.
func (h *Harness) saveLogsOnFailure() {
	if h.T != nil && h.T.Failed() {
		if err := h.saveLogs(); err != nil {
			h.Logf("failed to save container logs: %v", err)
		}
	}
}

// purgeDockerResources removes all containers and the network, cleaning up
// all Docker resources.
func (h *Harness) purgeDockerResources() {
	h.Log("Purging docker containers and network...")

	// Purge in same order as kill for consistency.
	h.purgeResource(h.electrs, "electrs")
	h.purgeResource(h.postgres, "postgres")
	h.purgeResource(h.tapd, "tapd")
	h.purgeResource(h.lnd, "lnd")
	h.purgeResource(h.bitcoind, "bitcoind")

	if h.network != nil {
		err := h.network.Close()
		if err != nil {
			h.Logf("failed to close network: %v", err)
		}
	}
}

// purgeResource removes a single Docker container resource.
func (h *Harness) purgeResource(
	res *dockertest.Resource, name string) {

	if res == nil {
		return
	}

	err := h.pool.Purge(res)
	if err != nil {
		h.Logf("failed to purge %s: %v", name, err)
	}
}

// cleanupArtifacts closes the log file and optionally removes the artifacts
// directory if the test passed and AlwaysKeepArtifacts is false.
func (h *Harness) cleanupArtifacts() {
	if h.harnessLogFile != nil {
		err := h.harnessLogFile.Close()
		if err != nil {
			h.Logf(
				"failed to close harness log file: %v", err,
			)
		}
	}

	// Keep artifacts by default for inspection (CI can disable with
	// AlwaysKeepArtifacts=false).
	if h.T != nil && !h.T.Failed() && !h.opts.AlwaysKeepArtifacts {
		_ = os.RemoveAll(h.artifactsDir)
	}
}

// createNetworkUnique creates a private Docker network with a unique name,
// retrying a few times on name collisions.
func (h *Harness) createNetworkUnique() (*dockertest.Network, error) {
	var lastErr error
	for i := 0; i < maxNetworkNameRetries; i++ {
		name := networkPrefix + randSuffix()
		netw, err := h.pool.CreateNetwork(name)
		if err == nil {
			return netw, nil
		}
		lastErr = err
		// Brief delay before retry to avoid tight retry loops and give
		// Docker time to clean up resources on name collision.
		time.Sleep(pollInterval)
	}

	return nil, lastErr
}

func (h *Harness) pruneStaleHarnessNetworks() {
	// Best-effort, ignore errors.
	nets, err := h.pool.Client.ListNetworks()
	if err != nil {
		h.Logf("[DEBUG] Failed to list networks for pruning: %v", err)
		return
	}

	for _, n := range nets {
		if !strings.HasPrefix(n.Name, networkPrefix) {
			continue
		}

		if len(n.Containers) == 0 {
			// Best-effort, ignore errors.
			err := h.pool.Client.RemoveNetwork(n.ID)
			if err != nil {
				h.Logf(
					"[DEBUG] Failed to remove stale "+
						"network %s: %v",
					n.Name, err,
				)
			}
		}
	}
}

// removeContainerByName attempts to remove a container by name. This is a
// best-effort operation used to clean up leftover containers from previous
// failed runs.
func (h *Harness) removeContainerByName(name string) {
	// Best-effort, ignore errors.
	containers, err := h.pool.Client.ListContainers(
		docker.ListContainersOptions{
			All: true,
			Filters: map[string][]string{
				"name": {name},
			},
		},
	)
	if err != nil {
		h.Logf("[DEBUG] Failed to list containers for cleanup: %v", err)
		return
	}

	for _, container := range containers {
		// Kill container if it's still running.
		err := h.pool.Client.KillContainer(docker.KillContainerOptions{
			ID: container.ID,
		})
		if err != nil {
			h.Logf("[DEBUG] Failed to kill container %s: %v",
				container.ID[:12], err)
		}

		// Remove the container.
		err = h.pool.Client.RemoveContainer(
			docker.RemoveContainerOptions{
				ID:    container.ID,
				Force: true,
			},
		)
		if err != nil {
			h.Logf("[DEBUG] Failed to remove container %s: %v",
				container.ID[:12], err)
		}
	}
}

// waitContainerRunning polls until the given container is running.
func (h *Harness) waitContainerRunning(res *dockertest.Resource) {
	// Poll container status until running, or timeout via dockertest retry.
	err := h.pool.Retry(func() error {
		inspect, err := h.pool.Client.InspectContainer(res.Container.ID)
		if err != nil {
			return err
		}
		if !inspect.State.Running {
			return fmt.Errorf("container not running yet")
		}

		return nil
	})

	require.NoError(h.T, err, "container not running: %s",
		res.Container.Name)
}

// saveLogs persists container logs to files in the artifacts directory.
func (h *Harness) saveLogs() error {
	if h.bitcoind != nil {
		_ = h.writeContainerLogsToFile(
			h.bitcoind,
			filepath.Join(h.artifactsDir, "bitcoind.log"),
		)
	}

	if h.lnd != nil {
		_ = h.writeContainerLogsToFile(
			h.lnd, filepath.Join(h.artifactsDir, "lnd.log"),
		)
	}

	if h.tapd != nil {
		_ = h.writeContainerLogsToFile(
			h.tapd, filepath.Join(h.artifactsDir, "tapd.log"),
		)
	}

	if h.electrs != nil {
		_ = h.writeContainerLogsToFile(
			h.electrs, filepath.Join(h.artifactsDir, "electrs.log"),
		)
	}

	if h.postgres != nil {
		_ = h.writeContainerLogsToFile(
			h.postgres, filepath.Join(
				h.artifactsDir, "postgres.log",
			),
		)
	}

	// Save logs for any additional LND instances.
	for name, inst := range h.extraLNDs {
		if inst != nil && inst.Resource != nil {
			_ = h.writeContainerLogsToFile(
				inst.Resource,
				filepath.Join(h.artifactsDir, name+".log"),
			)
		}
	}

	return nil
}

// writeContainerLogsToFile writes the full logs of a container to path.
func (h *Harness) writeContainerLogsToFile(res *dockertest.Resource,
	path string) error {

	var buf bytes.Buffer
	err := h.pool.Client.Logs(docker.LogsOptions{
		Container:    res.Container.ID,
		Stdout:       true,
		Stderr:       true,
		Tail:         "all",
		Follow:       false,
		OutputStream: &buf,
		ErrorStream:  &buf,
	})
	if err != nil {
		return err
	}

	return os.WriteFile(path, buf.Bytes(), 0o644)
}

// startBitcoind launches a bitcoind container and waits until its JSON-RPC is
// responsive then ensures a wallet exists.
func (h *Harness) startBitcoind() {
	// Remove any existing bitcoind container with the same name.
	containerName := h.containerName("bitcoin")
	h.removeContainerByName(containerName)

	cmd := []string{
		"-regtest",
		"-txindex=1",
		// Set wallet fallback fee to ~1 sat/vB so that even very low
		// estimates stay above bitcoind's relay floor.
		"-fallbackfee=0.00001",
		// Match the lower fee estimates we see from electrs in tests so
		// commitment packages targeting ~0.5 sat/vB still pass policy.
		"-minrelaytxfee=0.00000500",
		fmt.Sprintf("-rpcuser=%s", bitcoindRPCUser),
		fmt.Sprintf("-rpcpassword=%s", bitcoindRPCPass),
		"-rpcallowip=0.0.0.0/0",
		"-rpcbind=0.0.0.0",
		"-zmqpubrawblock=tcp://0.0.0.0:28332",
		"-zmqpubrawtx=tcp://0.0.0.0:28333",
		"-printtoconsole",
	}

	// Ensure absolute host path for bind mount.
	btcHostDir, err := filepath.Abs(h.bitcoinDataDir)
	require.NoError(h.T, err, "failed to get absolute path "+
		"for bitcoind data dir")

	res, err := h.pool.RunWithOptions(&dockertest.RunOptions{
		Repository: imageRepo(h.opts.BitcoindImage),
		Tag:        imageTag(h.opts.BitcoindImage),
		Cmd:        cmd,
		Env:        []string{},
		ExposedPorts: []string{
			"18443/tcp", "28332/tcp", "28333/tcp",
		},
		Name:     h.containerName("bitcoin"),
		Networks: []*dockertest.Network{h.network},
		Labels: map[string]string{
			"ark.harness":                h.group,
			"com.docker.compose.project": h.group,
		},
		Mounts: []string{
			fmt.Sprintf("%s:%s", btcHostDir,
				"/home/bitcoin/.bitcoin"),
		},
	}, func(hc *docker.HostConfig) {
		// Keep container for logs on failure; Purge() will clean up.
		hc.AutoRemove = false
		hc.PortBindings = map[docker.Port][]docker.PortBinding{
			"18443/tcp": {{HostIP: "0.0.0.0", HostPort: ""}},
			"28332/tcp": {{HostIP: "0.0.0.0", HostPort: ""}},
			"28333/tcp": {{HostIP: "0.0.0.0", HostPort: ""}},
		}
	})
	require.NoError(h.T, err, "failed to start bitcoind")
	h.bitcoind = res

	// LND will reach bitcoind by this container name inside the network.
	// Container name has leading '/', strip it.
	h.bitcoindName = res.Container.Name
	if len(h.bitcoindName) > 0 && h.bitcoindName[0] == '/' {
		h.bitcoindName = h.bitcoindName[1:]
	}

	// Log container info and resolve host ports.
	h.Logf("bitcoind container id=%s name=%s", res.Container.ID,
		res.Container.Name)

	// Wait until container is running before inspecting ports.
	h.waitContainerRunning(res)

	rpcPort := res.GetPort("18443/tcp")
	zmqBlock := res.GetPort("28332/tcp")
	zmqTx := res.GetPort("28333/tcp")
	h.BitcoindRPC = net.JoinHostPort("127.0.0.1", rpcPort)
	h.BitcoindZMQBlock = fmt.Sprintf("tcp://127.0.0.1:%s", zmqBlock)
	h.BitcoindZMQTx = fmt.Sprintf("tcp://127.0.0.1:%s", zmqTx)

	h.Logf("bitcoind RPC=%s ZMQ(block)=%s ZMQ(tx)=%s", h.BitcoindRPC,
		h.BitcoindZMQBlock, h.BitcoindZMQTx)

	// Ensure JSON-RPC is responsive before proceeding.
	h.Log("Waiting for bitcoind JSON-RPC to be responsive...")
	h.waitForBitcoind()

	// Ensure a wallet exists.
	h.Log("Ensuring bitcoind wallet exists...")
	h.bitcoindEnsureWallet()
}

// startElectrs launches an electrs container that serves an Esplora-compatible
// HTTP API used by arkd and tests. It shares the bitcoind datadir to access
// blocks and chainstate and connects to bitcoind over RPC inside the private
// network.
func (h *Harness) startElectrs() {
	// Remove any existing electrs container with the same name.
	containerName := h.containerName("electrs")
	h.removeContainerByName(containerName)

	// Electrs flags (match Esplora-compatible mode).
	cmd := []string{
		"-vvv",
		"--timestamp",
		"--network=regtest",
		fmt.Sprintf("--cookie=%s:%s", bitcoindRPCUser, bitcoindRPCPass),
		fmt.Sprintf("--daemon-rpc-addr=%s:18443", h.bitcoindName),
		"--http-addr=0.0.0.0:3002",
		"--electrum-rpc-addr=0.0.0.0:60401",
		"--cors=*",
		"--daemon-dir=/home/user/.bitcoin",
		"--db-dir=/home/user/.bitcoin/db",
	}

	// Mount the same host datadir used by bitcoind so electrs can read
	// blocks.
	btcHostDir, err := filepath.Abs(h.bitcoinDataDir)
	require.NoError(h.T, err, "failed to get absolute path "+
		"for bitcoind data dir")

	res, err := h.pool.RunWithOptions(&dockertest.RunOptions{
		Repository:   "mempool/electrs",
		Tag:          "latest",
		Cmd:          cmd,
		Env:          []string{"RUST_BACKTRACE=1"},
		ExposedPorts: []string{"3002/tcp", "60401/tcp"},
		Name:         h.containerName("electrs"),
		Networks:     []*dockertest.Network{h.network},
		Labels: map[string]string{
			"ark.harness":                h.group,
			"com.docker.compose.project": h.group,
		},
		Mounts: []string{
			fmt.Sprintf("%s:%s", btcHostDir, "/home/user/.bitcoin"),
		},
	}, func(hc *docker.HostConfig) {
		hc.AutoRemove = false
		hc.PortBindings = map[docker.Port][]docker.PortBinding{
			"3002/tcp":  {{HostIP: "0.0.0.0", HostPort: ""}},
			"60401/tcp": {{HostIP: "0.0.0.0", HostPort: ""}},
		}
		// Ensure explicit bind mount.
		hc.Binds = []string{
			fmt.Sprintf("%s:%s", btcHostDir, "/home/user/.bitcoin"),
		}
	})
	require.NoError(h.T, err, "failed to start electrs")
	h.Logf("electrs container id=%s name=%s", res.Container.ID,
		res.Container.Name)

	h.electrs = res

	// Resolve host port for HTTP (Esplora API).
	httpPort := res.GetPort("3002/tcp")
	h.EsploraURL = fmt.Sprintf("http://127.0.0.1:%s", httpPort)
	h.Logf("electrs HTTP=%s", h.EsploraURL)

	// Wait for Esplora endpoint to respond with current tip height.
	require.Eventually(h.T, func() bool {
		const connectTimeout = 2 * time.Second
		ctx, cancel := context.WithTimeout(
			context.Background(), connectTimeout,
		)
		defer cancel()

		req, err := http.NewRequestWithContext(
			ctx, http.MethodGet, h.EsploraURL+"/blocks/tip/height",
			nil,
		)
		if err != nil {
			return false
		}

		resp, err := harnessHTTPClient.Do(req)
		if err != nil {
			return false
		}
		defer resp.Body.Close()

		return resp.StatusCode == http.StatusOK
	}, defaultTimeout, pollInterval, "electrs HTTP not ready")
}

// startPostgres launches a postgres container for arkd to use instead of
// SQLite.
func (h *Harness) startPostgres() {
	// Remove any existing postgres container with the same name (from
	// previous failed runs).
	containerName := h.containerName("postgres")
	h.removeContainerByName(containerName)

	res, err := h.pool.RunWithOptions(&dockertest.RunOptions{
		Repository: "postgres",
		Tag:        "16-alpine",
		Env: []string{
			"POSTGRES_USER=ark",
			"POSTGRES_PASSWORD=ark",
			"POSTGRES_DB=ark",
		},
		ExposedPorts: []string{"5432/tcp"},
		Name:         h.containerName("postgres"),
		Networks:     []*dockertest.Network{h.network},
		Labels: map[string]string{
			"ark.harness":                h.group,
			"com.docker.compose.project": h.group,
		},
	}, func(hc *docker.HostConfig) {
		hc.AutoRemove = false
		hc.PortBindings = map[docker.Port][]docker.PortBinding{
			"5432/tcp": {{HostIP: "0.0.0.0", HostPort: ""}},
		}
	})
	require.NoError(h.T, err, "failed to start postgres")
	h.postgres = res

	h.Logf("postgres container id=%s name=%s", res.Container.ID,
		res.Container.Name)

	// Wait until container is running.
	h.waitContainerRunning(res)

	// Resolve host port.
	pgPort := res.GetPort("5432/tcp")
	h.PostgresHost = net.JoinHostPort("127.0.0.1", pgPort)
	h.Logf("postgres listening at %s", h.PostgresHost)

	// Wait for postgres to be ready.
	h.Log("Waiting for postgres to be ready...")
	require.Eventually(h.T, func() bool {
		conn, err := net.DialTimeout("tcp", h.PostgresHost, time.Second)
		if err != nil {
			return false
		}
		_ = conn.Close()

		return true
	}, defaultTimeout, pollInterval, "postgres not ready")
}

// Generate mines n regtest blocks to a fresh address by calling bitcoind's
// generatetoaddress RPC, returning block headers for test validation. This
// advances the chain immediately without requiring mempool transactions.
func (h *Harness) Generate(blocks int) []BlockHeader {
	h.T.Helper()

	addr := h.bitcoindGetNewAddress()
	headers := h.bitcoindGenerateToAddress(blocks, addr)

	h.Logf("Generated %d blocks to %s", blocks, addr)

	return headers
}

// Block is a mined block header along with the txids it contains.
type Block struct {
	// Header is the block header.
	Header BlockHeader

	// TxIDs are the txids in the block.
	TxIDs []string
}

// GenerateAndWait mines 'numBlocks' blocks and waits until wall-clock >= the
// last header time. It also returns the list of txids for each mined block (in
// the same order as headers).
func (h *Harness) GenerateAndWait(numBlocks int) []Block {
	headers := h.Generate(numBlocks)
	blocks := make([]Block, 0, len(headers))

	// Collect txids for each mined block.
	for _, header := range headers {
		txids, err := h.bitcoindGetBlockTxids(header.Hash)
		require.NoError(
			h.T, err, "getblock txids failed for %s", header.Hash,
		)

		blocks = append(blocks, Block{
			Header: header,
			TxIDs:  txids,
		})
	}

	last := headers[len(headers)-1]
	blockTime := time.Unix(last.Time, 0)
	if wait := time.Until(blockTime); wait > 0 {
		h.Logf("waiting %v until block time %v", wait, blockTime)

		// Add cushion beyond block time to ensure any time-dependent
		// operations have margin for clock skew and processing delays.
		time.Sleep(wait + blockTimeCushion)
	}

	return blocks
}

// BlockCount queries bitcoind's getblockcount RPC to retrieve the current
// regtest chain height, handling JSON response type variations to ensure
// robust parsing across different bitcoind versions and response formats.
func (h *Harness) BlockCount() uint32 {
	res, err := h.bitcoinRPCCall("getblockcount")
	require.NoError(h.T, err)
	var height uint32
	// bitcoind returns a number; json.Unmarshal into uint32 via float64
	// then cast.
	var raw any
	require.NoError(h.T, json.Unmarshal(res, &raw))
	switch v := raw.(type) {
	case float64:
		height = uint32(v)
	case int:
		height = uint32(v)
	default:
		// Attempt direct decode.
		require.NoError(h.T, json.Unmarshal(res, &height))
	}

	return height
}

// Faucet funds a test address by sending the specified amount from bitcoind's
// default wallet, creating unconfirmed UTXOs for tests to spend. This mimics
// external funding without requiring manual transaction construction.
func (h *Harness) Faucet(address string, amount btcutil.Amount) string {
	h.T.Helper()

	h.bitcoindEnsureWallet()
	txID := h.bitcoindSendToAddress(address, amount.ToBTC())
	h.Logf("Faucet sent %v to %s (txid %s)", amount, address, txID)

	return txID
}

// MempoolTxIDs queries bitcoind's getrawmempool RPC to retrieve all
// transaction IDs currently waiting in the mempool, enabling tests to verify
// transaction broadcast and propagation before confirmation.
func (h *Harness) MempoolTxIDs() []string {
	h.T.Helper()

	res, err := h.bitcoinRPCCall("getrawmempool")
	require.NoError(h.T, err)

	var txids []string
	require.NoError(
		h.T, json.Unmarshal(res, &txids),
		"getrawmempool unmarshal failed",
	)

	return txids
}

// WaitMempoolTxCount waits until the mempool has at least 'minTxCount' txs.
func (h *Harness) WaitMempoolTxCount(minTxCount int) []string {
	h.T.Helper()

	var txIDs []string
	// Poll more frequently (200ms) than default for mempool operations
	// since transaction propagation is typically fast.
	require.Eventually(
		h.T, func() bool {
			txIDs = h.MempoolTxIDs()

			return len(txIDs) >= minTxCount
		}, defaultTimeout, pollInterval,
		"mempool tx count %d < %d", len(txIDs), minTxCount,
	)

	return txIDs
}

// rpcRequest is a JSON-RPC request.
type rpcRequest struct {
	JSONRPC string        `json:"jsonrpc"`
	ID      int           `json:"id"`
	Method  string        `json:"method"`
	Params  []interface{} `json:"params"`
}

// rpcResponse is a JSON-RPC response.
type rpcResponse struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	ID int `json:"id"`
}

// BitcoinRPCClient returns a new bitcoind RPC client connected to the harness's
// bitcoind instance.
func (h *Harness) BitcoinRPCClient() (*rpcclient.Client, error) {
	connCfg := &rpcclient.ConnConfig{
		Host:         h.BitcoindRPC,
		User:         bitcoindRPCUser,
		Pass:         bitcoindRPCPass,
		HTTPPostMode: true,
		DisableTLS:   true,
	}

	return rpcclient.New(connCfg, nil)
}

// bitcoinRPCCall makes a JSON-RPC call to bitcoind and returns the raw result
// or an error.
func (h *Harness) bitcoinRPCCall(method string,
	params ...interface{}) (json.RawMessage, error) {

	ctxt, cancel := context.WithTimeout(
		context.Background(), defaultTimeout,
	)
	defer cancel()

	url := fmt.Sprintf("http://%s", h.BitcoindRPC)
	body, err := json.Marshal(rpcRequest{
		JSONRPC: "1.0", ID: 1, Method: method, Params: params,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal rpc request: %s: %w",
			method, err)
	}

	req, err := http.NewRequestWithContext(
		ctxt, http.MethodPost, url, bytes.NewReader(body),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create rpc request: %s: %w",
			method, err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(bitcoindRPCUser, bitcoindRPCPass)
	resp, err := harnessHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("rpc request failed: %s: %w", method,
			err)
	}

	defer resp.Body.Close()
	var rr rpcResponse
	err = json.NewDecoder(resp.Body).Decode(&rr)
	if err != nil {
		return nil, fmt.Errorf("rpc response decode failed: %s: %w",
			method, err)
	}

	if rr.Error != nil {
		return nil, fmt.Errorf("bitcoind error %d: %s (%s)",
			rr.Error.Code, rr.Error.Message, method)
	}

	return rr.Result, nil
}

// waitForBitcoind polls bitcoind's RPC until it responds or times out.
func (h *Harness) waitForBitcoind() {
	h.T.Helper()

	// Ensure node-level RPC is responsive.
	require.Eventually(h.T, func() bool {
		_, err := h.bitcoinRPCCall("getblockchaininfo")
		return err == nil
	}, defaultTimeout, time.Second, "bitcoind JSON-RPC not responsive")
}

// bitcoindEnsureWallet makes sure a default wallet exists, creating one if
// needed.
func (h *Harness) bitcoindEnsureWallet() {
	h.T.Helper()

	// Make sure a default wallet exists, create one if needed.
	res, err := h.bitcoinRPCCall("listwallets")
	require.NoError(h.T, err)

	var wallets []string
	err = json.Unmarshal(res, &wallets)
	require.NoError(h.T, err, "listwallets unmarshal failed")

	if len(wallets) == 0 {
		_, err = h.bitcoinRPCCall("createwallet", "default")
		require.NoError(h.T, err)
	}
}

// bitcoindGetNewAddress returns a new address from bitcoind's wallet.
func (h *Harness) bitcoindGetNewAddress() string {
	h.T.Helper()

	res, err := h.bitcoinRPCCall("getnewaddress")
	require.NoError(h.T, err)

	var addr string
	err = json.Unmarshal(res, &addr)
	require.NoError(h.T, err, "getnewaddress unmarshal failed")

	return addr
}

// BlockHeader is a minimal representation of a block header as returned by
// bitcoind's getblockheader RPC when verbose mode is enabled.
type BlockHeader struct {
	Hash              string  `json:"hash"`
	Confirmations     int64   `json:"confirmations,omitempty"`
	Height            int64   `json:"height"`
	Version           int64   `json:"version,omitempty"`
	VersionHex        string  `json:"versionHex,omitempty"`
	Merkleroot        string  `json:"merkleroot"`
	Time              int64   `json:"time"`
	Mediantime        int64   `json:"mediantime,omitempty"`
	Nonce             uint32  `json:"nonce,omitempty"`
	Bits              string  `json:"bits,omitempty"`
	Difficulty        float64 `json:"difficulty,omitempty"`
	Chainwork         string  `json:"chainwork,omitempty"`
	NTx               int64   `json:"nTx,omitempty"`
	Previousblockhash string  `json:"previousblockhash,omitempty"`
	Nextblockhash     string  `json:"nextblockhash,omitempty"`
}

// bitcoindGenerateToAddress mines blocks to the given address.
func (h *Harness) bitcoindGenerateToAddress(blocks int,
	address string) []BlockHeader {

	h.T.Helper()

	res, err := h.bitcoinRPCCall("generatetoaddress", blocks, address)
	require.NoError(h.T, err)

	var hashes []string
	err = json.Unmarshal(res, &hashes)
	require.NoError(h.T, err, "generatetoaddress unmarshal failed")

	headers := make([]BlockHeader, 0, len(hashes))
	for _, hash := range hashes {
		// Ask for verbose header JSON (true).
		hdrRes, err := h.bitcoinRPCCall("getblockheader", hash, true)
		require.NoError(h.T, err, "getblockheader rpc failed")

		var hdr BlockHeader
		err = json.Unmarshal(hdrRes, &hdr)
		require.NoError(h.T, err, "getblockheader unmarshal failed")

		headers = append(headers, hdr)
	}

	return headers
}

// bitcoindGetBlockTxids returns the list of txids for a given block hash.
func (h *Harness) bitcoindGetBlockTxids(hash string) ([]string, error) {
	// Setting verbosity=1 returns a JSON object with an array of txids as
	// strings.
	res, err := h.bitcoinRPCCall("getblock", hash, 1)
	if err != nil {
		return nil, err
	}

	var payload struct {
		Tx []string `json:"tx"`
	}
	if err := json.Unmarshal(res, &payload); err != nil {
		return nil, fmt.Errorf("getblock unmarshal failed: %w", err)
	}

	return payload.Tx, nil
}

// bitcoindSendToAddress sends amount BTC to address and returns the txid.
func (h *Harness) bitcoindSendToAddress(address string, amount float64) string {
	h.T.Helper()

	res, err := h.bitcoinRPCCall("sendtoaddress", address, amount)
	require.NoError(h.T, err)

	var txid string
	err = json.Unmarshal(res, &txid)
	require.NoError(h.T, err, "sendtoaddress unmarshal failed")

	return txid
}

// startLND launches an lnd container and waits until its gRPC is responsive
// and TLS cert and admin macaroon are available.
func (h *Harness) startLND() *LndInstance {
	inst := h.startLNDInstance("lnd", h.lndDataDir)
	h.lnd = inst.Resource
	h.LNDGRPCPort = inst.GRPCPort
	h.LNDRestPort = inst.RESTPort
	h.lndTLSCert = inst.TLSCert
	h.lndMacaroon = inst.Macaroon

	return inst
}

func (h *Harness) startLNDInstance(name, dataDir string) *LndInstance {
	h.T.Helper()

	require.NoError(h.T, os.MkdirAll(dataDir, 0o755))

	res := h.startLNDContainer(lndConfig{
		name:         name,
		dataDir:      dataDir,
		bitcoindName: h.bitcoindName,
		network:      h.network,
		group:        h.group,
		image:        imageRepo(h.opts.LNDImage),
		tag:          imageTag(h.opts.LNDImage),
	})

	inst := &LndInstance{
		Name:     name,
		Resource: res,
		DataDir:  dataDir,
		GRPCPort: res.GetPort("10009/tcp"),
		RESTPort: res.GetPort("8080/tcp"),
	}
	inst.ContainerName = strings.TrimPrefix(res.Container.Name, "/")
	inst.TLSCert = filepath.Join(dataDir, "tls.cert")
	inst.Macaroon = filepath.Join(
		dataDir, "data", "chain", "bitcoin", "regtest",
		"admin.macaroon",
	)

	h.Logf("%s gRPC=127.0.0.1:%s REST=127.0.0.1:%s", name,
		inst.GRPCPort, inst.RESTPort)

	require.Eventually(
		h.T, func() bool {
			_, err := os.Stat(inst.TLSCert)
			if err != nil {
				return false
			}

			addr := net.JoinHostPort("127.0.0.1", inst.GRPCPort)
			conn, err := net.DialTimeout("tcp", addr, time.Second)
			if err != nil {
				return false
			}

			_ = conn.Close()

			return true
		},
		defaultTimeout, time.Second,
		fmt.Sprintf("%s TLS/gRPC not ready", name),
	)

	return inst
}

// initAndWaitLND initializes the lnd wallet if needed and waits until it
// reaches SERVER_ACTIVE state.
func (h *Harness) initAndWaitLND(inst *LndInstance) {
	client := h.initAndWaitLNDInstance(inst)
	if inst != nil && inst.Name == "lnd" {
		h.LND = client
	}
}

func (h *Harness) initAndWaitLNDInstance(
	inst *LndInstance) *lndclient.LndServices {

	h.T.Helper()

	if inst == nil {
		inst = &LndInstance{
			Name:     "lnd",
			TLSCert:  h.lndTLSCert,
			Macaroon: h.lndMacaroon,
			GRPCPort: h.LNDGRPCPort,
		}
	}

	addr := net.JoinHostPort("127.0.0.1", inst.GRPCPort)
	tlsCert, err := credentials.NewClientTLSFromFile(inst.TLSCert, "")
	require.NoError(h.T, err, "failed to load %s TLS", inst.Name)

	h.Logf("Waiting for %s to reach SERVER_ACTIVE state...", inst.Name)
	require.Eventually(h.T, func() bool {
		const checkTimeout = 5 * time.Second

		ctxt, cancel := context.WithTimeout(
			context.Background(), checkTimeout,
		)
		defer cancel()

		conn, err := grpc.DialContext(
			ctxt, addr, grpc.WithTransportCredentials(tlsCert),
			grpc.WithBlock(),
		)
		if err != nil {
			return false
		}
		defer conn.Close()

		stateClient := lnrpc.NewStateClient(conn)
		resp, err := stateClient.GetState(
			ctxt, &lnrpc.GetStateRequest{},
		)
		if err != nil {
			return false
		}

		return resp.State == lnrpc.WalletState_SERVER_ACTIVE
	}, defaultTimeout, time.Second, fmt.Sprintf("%s not active", inst.Name))

	err = h.pool.Retry(func() error {
		if _, err := os.Stat(inst.Macaroon); err != nil {
			return err
		}

		return nil
	})
	require.NoError(h.T, err, "%s admin macaroon not found", inst.Name)

	lndClient, err := lndclient.NewLndServices(&lndclient.LndServicesConfig{
		LndAddress:         addr,
		CustomMacaroonPath: inst.Macaroon,
		TLSPath:            inst.TLSCert,
		Network:            lndclient.NetworkRegtest,
	})
	require.NoError(h.T, err, "failed to create %s client", inst.Name)

	services := &lndClient.LndServices
	inst.Client = services

	h.waitForLNDChainSyncInstance(inst)

	return services
}

// StartAdditionalLND launches an extra LND instance with the given name and
// returns its handle.
func (h *Harness) StartAdditionalLND(name string) *LndInstance {
	h.T.Helper()

	if name == "" {
		name = fmt.Sprintf("lnd-extra-%d", len(h.extraLNDs)+1)
	}

	if _, exists := h.extraLNDs[name]; exists {
		h.T.Fatalf("LND instance %s already exists", name)
	}

	dataDir := filepath.Join(h.artifactsDir, name)
	inst := h.startLNDInstance(name, dataDir)
	h.initAndWaitLNDInstance(inst)
	h.extraLNDs[name] = inst

	return inst
}

// GetAdditionalLND returns a previously started extra LND instance by name.
func (h *Harness) GetAdditionalLND(name string) *LndInstance {
	h.T.Helper()

	inst, exists := h.extraLNDs[name]
	if !exists {
		h.T.Fatalf("LND instance %s not found", name)
	}

	return inst
}

// WaitForLNDSync waits until LND reports it is synced to chain.
func (h *Harness) WaitForLNDChainSync() {
	h.T.Helper()

	if h.LND == nil {
		h.T.Fatalf("primary LND client not initialized")
	}

	h.waitForLNDChainSyncInstance(&LndInstance{Name: "lnd", Client: h.LND})
}

func (h *Harness) waitForLNDChainSyncInstance(inst *LndInstance) {
	h.T.Helper()

	// Poll frequently (200ms) for chain sync since it typically completes
	// quickly after wallet initialization.
	require.Eventually(
		h.T, func() bool {
			sync, err := inst.Client.Client.GetInfo(
				context.Background(),
			)
			if err != nil {
				return false
			}

			return sync.SyncedToChain
		},
		defaultTimeout, pollInterval,
		fmt.Sprintf("%s not synced", inst.Name),
	)
}

// SetupChannelBetween opens a channel from the local node towards the peer
// node with the specified capacity (in satoshis). The provided context controls
// the lifetime of the operation; if nil, context.Background is used.
func (h *Harness) SetupChannelBetween(local *LndInstance, peer *LndInstance,
	capacitySat, pushAmt int64) {

	t := h.T
	t.Helper()

	ctx := t.Context()

	if capacitySat == 0 {
		capacitySat = 500_000
	}

	// Ensure both nodes are synced to chain before opening channels.
	h.waitForLNDChainSyncInstance(local)
	h.waitForLNDChainSyncInstance(peer)

	peerInfo, err := peer.Client.Client.GetInfo(ctx)
	require.NoError(t, err, "getinfo failed for %s", peer.Name)

	peerKeyHex := fmt.Sprintf("%x", peerInfo.IdentityPubkey[:])

	// Create authenticated connection to local LND.
	localAddr := net.JoinHostPort("127.0.0.1", local.GRPCPort)
	localConn, err := getLNDClientConn(
		ctx, localAddr, local.TLSCert, local.Macaroon,
	)
	require.NoError(t, err, "failed to connect to %s gRPC", local.Name)

	defer localConn.Close()

	localRPC := lnrpc.NewLightningClient(localConn)
	addrResp, err := localRPC.NewAddress(ctx, &lnrpc.NewAddressRequest{
		Type: lnrpc.AddressType_WITNESS_PUBKEY_HASH,
	})
	require.NoError(t, err, "NewAddress failed for %s", local.Name)

	h.bitcoindSendToAddress(addrResp.Address, 1.0)
	h.Generate(6)

	// Wait for local LND to sync after generating blocks.
	h.waitForLNDChainSyncInstance(local)

	peerHost := fmt.Sprintf("%s:9735", peer.ContainerName)
	_, err = localRPC.ConnectPeer(ctx, &lnrpc.ConnectPeerRequest{
		Addr: &lnrpc.LightningAddress{
			Pubkey: peerKeyHex,
			Host:   peerHost,
		},
		Perm: true,
	})
	require.NoError(t, err, "ConnectPeer failed for %s -> %s",
		local.Name, peer.Name)

	// Wait for peer connection to be fully established. Poll frequently
	// (200ms) since peer connections typically establish quickly.
	require.Eventually(h.T, func() bool {
		resp, err := localRPC.ListPeers(ctx, &lnrpc.ListPeersRequest{})
		if err != nil {
			return false
		}
		for _, p := range resp.Peers {
			if p.PubKey == peerKeyHex {
				return true
			}
		}

		return false
	}, defaultTimeout, pollInterval, "peer not connected")

	_, err = localRPC.OpenChannelSync(ctx, &lnrpc.OpenChannelRequest{
		NodePubkey:         peerInfo.IdentityPubkey[:],
		LocalFundingAmount: capacitySat,
		PushSat:            pushAmt,
	})
	require.NoError(t, err, "OpenChannel failed for %s -> %s",
		local.Name, peer.Name)

	h.Generate(6)

	require.Eventually(t, func() bool {
		resp, err := localRPC.ListChannels(
			ctx, &lnrpc.ListChannelsRequest{},
		)
		if err != nil {
			return false
		}

		return len(resp.Channels) > 0
	}, defaultTimeout, pollInterval)
}

// startTapd launches a tapd container that connects to the LND instance.
func (h *Harness) startTapd() {
	h.tapd = h.startTapdContainer(tapdConfig{
		name:         "tapd",
		tapdDataDir:  h.tapdDataDir,
		lndDataDir:   h.lndDataDir,
		lndContainer: h.lnd,
		network:      h.network,
		group:        h.group,
		image:        imageRepo(h.opts.TapdImage),
		tag:          imageTag(h.opts.TapdImage),
	})

	h.TapdGRPCPort = h.tapd.GetPort("10029/tcp")
	h.TapdRestPort = h.tapd.GetPort("8089/tcp")
	h.Logf("tapd gRPC=127.0.0.1:%s REST=127.0.0.1:%s", h.TapdGRPCPort,
		h.TapdRestPort)

	// Set paths to tapd TLS cert and macaroon for client connections.
	h.tapdTLSCert = filepath.Join(h.tapdDataDir, "tls.cert")
	h.tapdMacaroon = filepath.Join(
		h.tapdDataDir, "data", "regtest", "admin.macaroon",
	)

	h.Log("Waiting for tapd to be ready and synced...")
	h.waitForTapdReady()
	h.Log("tapd is ready and synced")
}

// getLNDClientConn creates a gRPC connection to LND using TLS and macaroon
// authentication.
func getLNDClientConn(ctx context.Context, addr, tlsPath,
	macaroonPath string) (*grpc.ClientConn, error) {

	// Load TLS credentials.
	creds, err := credentials.NewClientTLSFromFile(tlsPath, "")
	if err != nil {
		return nil, fmt.Errorf("failed to load TLS cert: %w", err)
	}

	// Load macaroon.
	macBytes, err := os.ReadFile(macaroonPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read macaroon: %w", err)
	}
	mac := &macaroon.Macaroon{}
	if err := mac.UnmarshalBinary(macBytes); err != nil {
		return nil, fmt.Errorf("failed to unmarshal macaroon: %w", err)
	}

	macaroonCred, err := macaroons.NewMacaroonCredential(mac)
	if err != nil {
		return nil, fmt.Errorf("failed to create macaroon credential: "+
			"%w", err)
	}

	// Create dial options with TLS and macaroon credentials.
	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(creds),
		grpc.WithPerRPCCredentials(macaroonCred),
		grpc.WithBlock(),
	}

	conn, err := grpc.DialContext(
		ctx, addr, opts...,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to dial LND: %w", err)
	}

	return conn, nil
}

// getTapdClientConn creates a gRPC connection to tapd using TLS and macaroon
// authentication.
func getTapdClientConn(ctx context.Context, addr, tlsPath,
	macaroonPath string) (*grpc.ClientConn, error) {

	// Load TLS credentials.
	creds, err := credentials.NewClientTLSFromFile(tlsPath, "")
	if err != nil {
		return nil, fmt.Errorf("failed to load TLS cert: %w", err)
	}

	// Load macaroon.
	macBytes, err := os.ReadFile(macaroonPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read macaroon: %w", err)
	}
	mac := &macaroon.Macaroon{}
	if err := mac.UnmarshalBinary(macBytes); err != nil {
		return nil, fmt.Errorf("failed to unmarshal macaroon: %w", err)
	}

	macaroonCred, err := macaroons.NewMacaroonCredential(mac)
	if err != nil {
		return nil, fmt.Errorf("failed to create macaroon credential: "+
			"%w", err)
	}

	// Create dial options with TLS and macaroon credentials.
	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(creds),
		grpc.WithPerRPCCredentials(macaroonCred),
		grpc.WithBlock(),
	}

	conn, err := grpc.DialContext(ctx, addr, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to dial tapd: %w", err)
	}

	return conn, nil
}

// waitForTapdReady waits until tapd's GetInfo RPC responds and reports that
// tapd is synced to chain.
func (h *Harness) waitForTapdReady() {
	h.T.Helper()

	// Wait for TLS cert and macaroon to be created by tapd.
	require.Eventually(h.T, func() bool {
		_, errCert := os.Stat(h.tapdTLSCert)
		_, errMac := os.Stat(h.tapdMacaroon)
		return errCert == nil && errMac == nil
	}, defaultTimeout, time.Second, "tapd TLS cert or macaroon not found")

	// Now wait for tapd to be ready and synced.
	require.Eventually(h.T, func() bool {
		const checkTimeout = 5 * time.Second
		ctx, cancel := context.WithTimeout(
			context.Background(), checkTimeout,
		)
		defer cancel()

		addr := net.JoinHostPort("127.0.0.1", h.TapdGRPCPort)
		conn, err := getTapdClientConn(
			ctx, addr, h.tapdTLSCert, h.tapdMacaroon,
		)
		if err != nil {
			return false
		}
		defer conn.Close()

		// Call GetInfo to check if tapd is ready and synced.
		client := taprpc.NewTaprootAssetsClient(conn)
		resp, err := client.GetInfo(ctx, &taprpc.GetInfoRequest{})
		if err != nil {
			return false
		}

		// Check if tapd is synced to chain.
		return resp.SyncToChain
	}, defaultTimeout, time.Second, "tapd not ready or not synced")
}

// imageRepo extracts the repository from a Docker image reference.
func imageRepo(image string) string {
	// "repo:tag" -> repo
	for i := len(image) - 1; i >= 0; i-- {
		if image[i] == ':' {
			return image[:i]
		}
	}

	return image
}

// imageTag extracts the tag from a Docker image reference ("" if none).
func imageTag(image string) string {
	// "repo:tag" -> tag (empty if none)
	for i := len(image) - 1; i >= 0; i-- {
		if image[i] == ':' {
			return image[i+1:]
		}
	}

	return ""
}

// containerName returns a container name, optionally with random suffix. If the
// harness has an explicit GroupName, use it without suffix for predictable
// names.
func (h *Harness) containerName(prefix string) string {
	if h.opts.GroupName != "" {
		return prefix + "-" + h.group
	}

	return prefix + "-" + h.group + "-" + randSuffix()
}

// randSuffix returns a short, random suffix suitable for naming resources.
func randSuffix() string {
	const alpha = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 8)

	if _, err := crand.Read(b); err != nil {
		// fallback to time-based if RNG fails
		now := time.Now().UnixNano()
		for i := range b {
			b[i] = alpha[int(now+int64(i))%len(alpha)]
		}

		return string(b)
	}

	for i := range b {
		b[i] = alpha[int(b[i])%len(alpha)]
	}

	return string(b)
}

// NewTapClientHarness creates a TapClientHarness connected to the harness's
// main tapd instance. This is a stub for now as TapClientHarness is not yet
// ported.
func (h *Harness) NewTapClientHarness(name string) interface{} {
	// TODO: Port TapClientHarness from tap-arktree when needed.
	h.T.Fatal("NewTapClientHarness not yet implemented")

	return nil
}

// SetPostgresEnabled allows tests to programmatically enable or disable
// postgres. It returns the previous value so tests can restore it. This is
// useful for testing postgres-specific functionality without requiring command
// line flags.
func SetPostgresEnabled(enabled bool) bool {
	old := *harnessPostgres
	*harnessPostgres = enabled

	return old
}

// TapdUniverseHost returns the universe host for the harness's main tapd,
// suitable for container-to-container communication.
func (h *Harness) TapdUniverseHost() string {
	// Get the container name (strip leading '/' if present).
	tapdName := h.tapd.Container.Name
	if len(tapdName) > 0 && tapdName[0] == '/' {
		tapdName = tapdName[1:]
	}

	// Use container name with internal port for container-to-container
	// communication.
	return net.JoinHostPort(tapdName, "10029")
}
