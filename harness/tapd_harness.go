package harness

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/taproot-assets/taprpc"
	lnrpc "github.com/lightningnetwork/lnd/lnrpc"
	"github.com/ory/dockertest/v3"
	"github.com/ory/dockertest/v3/docker"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

// TapdHarness represents a paired LND and tapd instance for test clients.
type TapdHarness struct {
	// Name is the identifier for this harness (e.g., "alice", "bob").
	Name string

	// h is the parent harness.
	h *Harness

	// lnd is the lnd container.
	lnd *dockertest.Resource

	// tapd is the tapd container.
	tapd *dockertest.Resource

	// lndDataDir is the lnd data dir.
	lndDataDir string

	// tapdDataDir is the tapd data dir.
	tapdDataDir string

	// LNDGRPCPort is the host port mapped to lnd gRPC.
	LNDGRPCPort string

	// LNDRestPort is the host port mapped to lnd REST.
	LNDRestPort string

	// TapdGRPCPort is the host port mapped to tapd gRPC.
	TapdGRPCPort string

	// TapdRestPort is the host port mapped to tapd REST.
	TapdRestPort string

	// LNDTLSCert is the path to the LND TLS cert.
	LNDTLSCert string

	// LNDMacaroon is the path to the LND admin macaroon.
	LNDMacaroon string

	// TapdTLSCert is the path to the tapd TLS cert.
	TapdTLSCert string

	// TapdMacaroon is the path to the tapd admin macaroon.
	TapdMacaroon string

	// LND is the lndclient instance connected to the running LND.
	LND *lndclient.LndServices
}

// lndConfig holds the configuration for starting an LND container.
type lndConfig struct {
	name         string
	dataDir      string
	bitcoindName string
	network      *dockertest.Network
	group        string
	image        string
	tag          string

	// chainBackend selects lnd's chain backend: LNDChainBackendBitcoind
	// (default) or LNDChainBackendNeutrino. Neutrino backs lnd via SPV over
	// the regtest bitcoind's P2P + compact filters, exercising lnd's native
	// 1p1c broadcast path instead of bitcoind's synchronous mempool checks.
	chainBackend string
}

// tapdConfig holds the configuration for starting a tapd container.
type tapdConfig struct {
	name         string
	tapdDataDir  string
	lndDataDir   string
	lndContainer *dockertest.Resource
	network      *dockertest.Network
	group        string
	image        string
	tag          string
}

// NewTapdHarness creates a new LND + tapd harness for test clients. The LND
// instance will connect to the main harness's bitcoind, and the tapd instance
// will connect to the new LND instance.
func (h *Harness) NewTapdHarness(name string) *TapdHarness {
	h.T.Helper()

	th := &TapdHarness{
		Name: name,
		h:    h,
	}

	// Create data directories for this harness.
	th.lndDataDir = filepath.Join(h.artifactsDir, name+"_lnd")
	th.tapdDataDir = filepath.Join(h.artifactsDir, name+"_tapd")
	require.NoError(h.T, os.MkdirAll(th.lndDataDir, 0o755))
	require.NoError(h.T, os.MkdirAll(th.tapdDataDir, 0o755))

	h.Logf("Starting LND for %s...", name)
	th.lnd = h.startLNDContainer(lndConfig{
		name:         name + "-lnd",
		dataDir:      th.lndDataDir,
		bitcoindName: h.bitcoindName,
		network:      h.network,
		group:        h.group,
		image:        imageRepo(h.opts.LNDImage),
		tag:          imageTag(h.opts.LNDImage),
	})
	th.setupLNDPaths()

	h.Logf("Initializing LND wallet for %s...", name)
	th.initAndWaitLND()

	h.Logf("Starting tapd for %s...", name)
	th.tapd = h.startTapdContainer(tapdConfig{
		name:         name + "-tapd",
		tapdDataDir:  th.tapdDataDir,
		lndDataDir:   th.lndDataDir,
		lndContainer: th.lnd,
		network:      h.network,
		group:        h.group,
		image:        imageRepo(h.opts.TapdImage),
		tag:          imageTag(h.opts.TapdImage),
	})
	th.setupTapdPaths()

	h.Logf("TapdHarness %s is ready", name)

	return th
}

// startLNDContainer starts an LND container with the given configuration.
func (h *Harness) startLNDContainer(cfg lndConfig) *dockertest.Resource {
	lndDirInContainer := "/data"
	lndName := h.containerName(cfg.name)
	user := fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())

	// Remove any existing LND container with the same name.
	h.removeContainerByName(lndName)

	cmd := []string{
		fmt.Sprintf("--lnddir=%s", lndDirInContainer),
		"--bitcoin.active",
		"--bitcoin.regtest",
		"--rpclisten=0.0.0.0:10009",
		"--restlisten=0.0.0.0:8080",
		"--listen=0.0.0.0:9735",
		fmt.Sprintf("--tlsextradomain=%s", lndName),
		fmt.Sprintf("--tlsextraip=%s", lndName),
		"--noseedbackup",
		"--nobootstrap",
		"--accept-keysend",
		"--protocol.option-scid-alias",
		"--protocol.zero-conf",
	}

	// Append the chain-backend-specific flags. Neutrino syncs and
	// broadcasts over the regtest bitcoind's P2P interface (which serves
	// compact block filters), so lnd uses its SPV broadcast path; this is
	// what lets us observe whether an lnd-backed daemon can relay zero-fee
	// v3 anchor packages via 1p1c instead of a direct bitcoind submitter.
	switch cfg.chainBackend {
	case LNDChainBackendNeutrino:
		cmd = append(
			cmd, "--bitcoin.node=neutrino",
			fmt.Sprintf("--neutrino.connect=%s:18444",
				cfg.bitcoindName),
		)

	default:
		cmd = append(
			cmd, "--bitcoin.node=bitcoind",
			fmt.Sprintf("--bitcoind.rpchost=%s:18443",
				cfg.bitcoindName),
			fmt.Sprintf("--bitcoind.rpcuser=%s", BitcoindRPCUser),
			fmt.Sprintf("--bitcoind.rpcpass=%s", BitcoindRPCPass),
			fmt.Sprintf("--bitcoind.zmqpubrawblock=tcp://%s:28332",
				cfg.bitcoindName),
			fmt.Sprintf("--bitcoind.zmqpubrawtx=tcp://%s:28333",
				cfg.bitcoindName),
		)
	}

	lndHostDir, err := filepath.Abs(cfg.dataDir)
	require.NoError(
		h.T, err, "failed to get absolute path for lnd data dir",
	)

	res, err := h.runWithPortBindRetry(lndName, func() (
		*dockertest.Resource, error) {

		return h.pool.RunWithOptions(&dockertest.RunOptions{
			Repository:   cfg.image,
			Tag:          cfg.tag,
			Cmd:          cmd,
			ExposedPorts: []string{"10009/tcp", "8080/tcp"},
			Name:         lndName,
			Networks:     []*dockertest.Network{cfg.network},
			User:         user,
			Mounts: []string{
				fmt.Sprintf("%s:%s", lndHostDir,
					lndDirInContainer),
			},
			Labels: map[string]string{
				"ark.harness":                cfg.group,
				"com.docker.compose.project": cfg.group,
			},
		}, func(hc *docker.HostConfig) {
			hc.AutoRemove = false
			hc.PortBindings =
				map[docker.Port][]docker.PortBinding{
					"10009/tcp": {{
						HostIP:   "0.0.0.0",
						HostPort: "",
					}},
					"8080/tcp": {{
						HostIP:   "0.0.0.0",
						HostPort: "",
					}},
				}

			hc.Binds = []string{
				fmt.Sprintf("%s:%s", lndHostDir,
					lndDirInContainer),
			}
		})
	})
	require.NoError(h.T, err, "failed to start lnd for "+cfg.name)

	h.Logf(
		"%s LND container id=%s name=%s", cfg.name, res.Container.ID,
		res.Container.Name,
	)

	h.waitContainerRunning(res)

	return res
}

// startTapdContainer starts a tapd container with the given configuration.
func (h *Harness) startTapdContainer(cfg tapdConfig) *dockertest.Resource {
	// Use /data/tapd as the base directory to avoid requiring root
	// privileges, matching the pattern used for LND containers.
	tapdDirInContainer := "/data/tapd"

	// Get LND's container name for internal network connectivity.
	lndName := cfg.lndContainer.Container.Name
	if len(lndName) > 0 && lndName[0] == '/' {
		lndName = lndName[1:]
	}

	tapdName := h.containerName(cfg.name)
	user := fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())

	// Remove any existing tapd container with the same name.
	h.removeContainerByName(tapdName)

	cmd := []string{
		fmt.Sprintf("--tapddir=%s", tapdDirInContainer),
		"--network=regtest",
		fmt.Sprintf("--lnd.host=%s:10009", lndName),
		fmt.Sprintf("--lnd.macaroonpath=%s",
			"/lnd/data/chain/bitcoin/regtest/admin.macaroon"),
		fmt.Sprintf("--lnd.tlspath=%s", "/lnd/tls.cert"),
		"--rpclisten=0.0.0.0:10029",
		"--restlisten=0.0.0.0:8089",
		fmt.Sprintf("--tlsextradomain=%s", tapdName),
		fmt.Sprintf("--tlsextraip=%s", tapdName),
		"--universe.syncinterval=10s",
		"--allow-public-stats",
		"--allow-public-uni-proof-courier",
		"--universe.public-access=rw",
		"--universe.sync-all-assets",
		"--debuglevel=debug",
	}

	tapdHostDir, err := filepath.Abs(cfg.tapdDataDir)
	require.NoError(
		h.T, err, "failed to get absolute path for tapd data dir",
	)

	lndHostDir, err := filepath.Abs(cfg.lndDataDir)
	require.NoError(
		h.T, err, "failed to get absolute path for lnd data dir",
	)

	res, err := h.runWithPortBindRetry(tapdName, func() (
		*dockertest.Resource, error) {

		return h.pool.RunWithOptions(&dockertest.RunOptions{
			Repository:   cfg.image,
			Tag:          cfg.tag,
			Cmd:          cmd,
			Env:          []string{"RUST_BACKTRACE=1"},
			ExposedPorts: []string{"10029/tcp", "8089/tcp"},
			Name:         tapdName,
			Networks:     []*dockertest.Network{cfg.network},
			// Run as current user to avoid permission issues.
			User: user,
			Mounts: []string{
				fmt.Sprintf("%s:%s", tapdHostDir,
					tapdDirInContainer),
				fmt.Sprintf("%s:%s:ro", lndHostDir, "/lnd"),
			},
			Labels: map[string]string{
				"ark.harness":                cfg.group,
				"com.docker.compose.project": cfg.group,
			},
		}, func(hc *docker.HostConfig) {
			hc.AutoRemove = false
			hc.PortBindings =
				map[docker.Port][]docker.PortBinding{
					"10029/tcp": {{
						HostIP:   "0.0.0.0",
						HostPort: "",
					}},
					"8089/tcp": {{
						HostIP:   "0.0.0.0",
						HostPort: "",
					}},
				}

			hc.Binds = []string{
				fmt.Sprintf("%s:%s", tapdHostDir,
					tapdDirInContainer),
				fmt.Sprintf("%s:%s:ro", lndHostDir, "/lnd"),
			}
		})
	})
	require.NoError(h.T, err, "failed to start tapd for "+cfg.name)

	h.Logf(
		"%s tapd container id=%s name=%s", cfg.name, res.Container.ID,
		res.Container.Name,
	)

	h.waitContainerRunning(res)

	return res
}

// setupLNDPaths sets up the LND-related paths and ports for the harness.
func (th *TapdHarness) setupLNDPaths() {
	th.LNDGRPCPort = th.lnd.GetPort("10009/tcp")
	th.LNDRestPort = th.lnd.GetPort("8080/tcp")
	th.h.Logf(
		"%s lnd gRPC=127.0.0.1:%s REST=127.0.0.1:%s", th.Name,
		th.LNDGRPCPort, th.LNDRestPort,
	)

	th.LNDTLSCert = filepath.Join(th.lndDataDir, "tls.cert")
	th.LNDMacaroon = filepath.Join(
		th.lndDataDir, "data", "chain", "bitcoin", "regtest",
		"admin.macaroon",
	)

	// Wait for TLS cert to be available.
	require.Eventually(th.h.T, func() bool {
		if !lndTLSReady(th.LNDTLSCert) {
			return false
		}

		addr := net.JoinHostPort("127.0.0.1", th.LNDGRPCPort)
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err != nil {
			return false
		}

		_ = conn.Close()

		return true
	}, defaultTimeout, time.Second, th.Name+" LND TLS/gRPC not ready")
}

// setupTapdPaths sets up the tapd-related paths and ports for the harness.
func (th *TapdHarness) setupTapdPaths() {
	th.TapdGRPCPort = th.tapd.GetPort("10029/tcp")
	th.TapdRestPort = th.tapd.GetPort("8089/tcp")
	th.h.Logf(
		"%s tapd gRPC=127.0.0.1:%s REST=127.0.0.1:%s", th.Name,
		th.TapdGRPCPort, th.TapdRestPort,
	)

	th.TapdTLSCert = filepath.Join(th.tapdDataDir, "tls.cert")
	th.TapdMacaroon = filepath.Join(
		th.tapdDataDir, "data", "regtest", "admin.macaroon",
	)

	th.h.Logf("Waiting for %s tapd to be ready and synced...", th.Name)
	th.waitForTapdReady()
	th.h.Logf("%s tapd is ready and synced", th.Name)
}

// initAndWaitLND initializes the LND wallet and waits until it's active.
func (th *TapdHarness) initAndWaitLND() {
	addr := net.JoinHostPort("127.0.0.1", th.LNDGRPCPort)
	// Wait for LND's state service to report SERVER_ACTIVE.
	th.h.Logf("Waiting for %s LND to reach SERVER_ACTIVE state...", th.Name)
	require.Eventually(th.h.T, func() bool {
		const checkTimeout = 5 * time.Second

		tlsCert, err := loadClientTLSCredentials(th.LNDTLSCert)
		if err != nil {
			return false
		}

		ctx, cancel := context.WithTimeout(
			context.Background(), checkTimeout,
		)
		defer cancel()

		conn, err := grpc.NewClient(
			addr, grpc.WithTransportCredentials(tlsCert),
		)
		if err != nil {
			return false
		}
		defer conn.Close()

		stateClient := lnrpc.NewStateClient(conn)
		resp, err := stateClient.GetState(
			ctx, &lnrpc.GetStateRequest{},
		)
		if err != nil {
			return false
		}

		return resp.State == lnrpc.WalletState_SERVER_ACTIVE
	}, defaultTimeout, time.Second, th.Name+" LND not active")

	// Wait for macaroon file.
	err := th.h.pool.Retry(func() error {
		if _, err := os.Stat(th.LNDMacaroon); err != nil {
			return err
		}

		return nil
	})
	require.NoError(th.h.T, err, th.Name+" LND admin macaroon not found")

	// Create lndclient.
	lndClient, err := lndclient.NewLndServices(
		&lndclient.LndServicesConfig{
			LndAddress:         addr,
			CustomMacaroonPath: th.LNDMacaroon,
			TLSPath:            th.LNDTLSCert,
			Network:            lndclient.NetworkRegtest,
		},
	)
	require.NoError(th.h.T, err, "failed to create "+th.Name+" lnd client")
	th.LND = &lndClient.LndServices

	// Wait for chain sync. Poll frequently (200ms) since chain sync
	// typically completes quickly after wallet initialization.
	require.Eventually(th.h.T, func() bool {
		sync, err := th.LND.Client.GetInfo(context.Background())
		if err != nil {
			return false
		}

		return sync.SyncedToChain
	}, defaultTimeout, pollInterval, th.Name+" LND not synced")
}

// waitForTapdReady waits until tapd is ready and synced to chain.
func (th *TapdHarness) waitForTapdReady() {
	// Wait for TLS cert and macaroon to be created.
	require.Eventually(th.h.T, func() bool {
		_, errCert := os.Stat(th.TapdTLSCert)
		_, errMac := os.Stat(th.TapdMacaroon)

		return errCert == nil && errMac == nil
	}, defaultTimeout, time.Second,
		th.Name+" tapd TLS cert or macaroon not found")

	// Wait for tapd to be ready and synced.
	require.Eventually(th.h.T, func() bool {
		const checkTimeout = 5 * time.Second
		ctx, cancel := context.WithTimeout(
			context.Background(), checkTimeout,
		)
		defer cancel()

		addr := net.JoinHostPort("127.0.0.1", th.TapdGRPCPort)
		conn, err := getTapdClientConn(
			ctx, addr, th.TapdTLSCert, th.TapdMacaroon,
		)
		if err != nil {
			return false
		}
		defer conn.Close()

		client := taprpc.NewTaprootAssetsClient(conn)
		resp, err := client.GetInfo(ctx, &taprpc.GetInfoRequest{})
		if err != nil {
			return false
		}

		return resp.SyncToChain
	}, defaultTimeout, time.Second, th.Name+" tapd not ready or not synced")
}

// Stop tears down the LND and tapd containers for this harness.
func (th *TapdHarness) Stop() {
	th.h.Logf("Stopping %s TapdHarness...", th.Name)

	if th.tapd != nil {
		err := th.h.pool.Client.KillContainer(
			docker.KillContainerOptions{
				ID: th.tapd.Container.ID,
			},
		)
		if err != nil {
			th.h.Logf("failed to kill %s tapd: %v", th.Name, err)
		} else {
			th.h.Logf("%s tapd killed", th.Name)
		}
	}

	if th.lnd != nil {
		err := th.h.pool.Client.KillContainer(
			docker.KillContainerOptions{
				ID: th.lnd.Container.ID,
			},
		)
		if err != nil {
			th.h.Logf("failed to kill %s lnd: %v", th.Name, err)
		} else {
			th.h.Logf("%s lnd killed", th.Name)
		}
	}

	if th.tapd != nil {
		err := th.h.pool.Purge(th.tapd)
		if err != nil {
			th.h.Logf("failed to purge %s tapd: %v", th.Name, err)
		}
	}

	if th.lnd != nil {
		err := th.h.pool.Purge(th.lnd)
		if err != nil {
			th.h.Logf("failed to purge %s lnd: %v", th.Name, err)
		}
	}

	th.h.Logf("%s TapdHarness stopped", th.Name)
}

// NewTapClientHarness creates a TapClientHarness connected to this
// TapdHarness's tapd instance. This is a stub for now as TapClientHarness
// is not yet ported.
func (th *TapdHarness) NewTapClientHarness() interface{} {
	// TODO: Port TapClientHarness from tap-arktree when needed.
	th.h.T.Fatal("NewTapClientHarness not yet implemented")

	return nil
}
