//go:build !js || !wasm

package btcwbackend

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/btcsuite/btcwallet/chain"
	basewallet "github.com/btcsuite/btcwallet/wallet"
	"github.com/btcsuite/btcwallet/walletdb"
	_ "github.com/btcsuite/btcwallet/walletdb/bdb" // Register bdb backend.
	"github.com/lightninglabs/neutrino"
	"github.com/lightningnetwork/lnd/blockcache"
)

// neutrinoDBName is the filename for the neutrino bbolt database.
const neutrinoDBName = "neutrino.db"

// defaultBlockCacheSize is the maximum size in bytes of the in-memory
// block cache. This prevents redundant block fetches during wallet
// sync, chain notification processing, and TxProof construction.
// lnd uses 20 MiB; we use a smaller default since the light client
// only fetches blocks on demand.
const defaultBlockCacheSize uint64 = 2 * 1024 * 1024 // 2 MiB

// defaultDBTimeout is the default timeout for opening the neutrino
// bbolt database.
const defaultDBTimeout = 60 * time.Second

// NeutrinoServiceOption configures a NeutrinoService.
type NeutrinoServiceOption func(*NeutrinoService)

// NeutrinoService manages the lifecycle of a neutrino ChainService.
// It handles database creation, peer configuration, and exposes the
// chain service for use by btcwallet and the chain backend.
type NeutrinoService struct {
	// cs is the running neutrino chain service.
	cs *neutrino.ChainService

	// db is the walletdb backing neutrino's header and filter state.
	db walletdb.DB

	// blockCache is the shared LRU block cache used by both neutrino
	// and the NeutrinoNotifier to avoid duplicate block fetches.
	blockCache *blockcache.BlockCache

	// chainParams identifies the Bitcoin network.
	chainParams *chaincfg.Params

	// log is the structured logger.
	log btclog.Logger

	// wireGlobalLoggers controls whether third-party package globals are
	// wired to log. These dependencies do not expose instance-level
	// loggers, so this is enabled by default for normal one-daemon-per-
	// process usage and disabled by parallel in-process tests.
	wireGlobalLoggers bool
}

// WithGlobalDependencyLoggers controls whether NeutrinoService.Start wires the
// neutrino and btcwallet package-global loggers to this service's logger.
func WithGlobalDependencyLoggers(enabled bool) NeutrinoServiceOption {
	return func(n *NeutrinoService) {
		n.wireGlobalLoggers = enabled
	}
}

// WithoutGlobalDependencyLoggers disables wiring the neutrino and btcwallet
// package-global loggers to this service's logger.
func WithoutGlobalDependencyLoggers() NeutrinoServiceOption {
	return WithGlobalDependencyLoggers(false)
}

// NewNeutrinoService creates a new neutrino chain service from the
// given configuration. The service is NOT started — call Start()
// after construction.
func NewNeutrinoService(dataDir string, chainParams *chaincfg.Params,
	connectPeers, addPeers []string, persistFilters bool,
	blockHeadersSource, filterHeadersSource string, logger btclog.Logger,
	opts ...NeutrinoServiceOption) (*NeutrinoService, error) {

	if chainParams == nil {
		return nil, fmt.Errorf("chain params are required")
	}

	headersImport, err := neutrinoHeadersImportConfig(
		blockHeadersSource, filterHeadersSource,
	)
	if err != nil {
		return nil, err
	}

	// Ensure the data directory exists before attempting to
	// open or create the bbolt database. The daemon's
	// initDatabase creates the network directory, but callers
	// like preStartNeutrino may resolve a custom path that
	// does not yet exist.
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create neutrino data dir: %w", err)
	}

	dbPath := filepath.Join(dataDir, neutrinoDBName)

	// Try to open an existing DB first (daemon restart), falling
	// back to creating a new one (first run).
	db, err := walletdb.Open(
		"bdb", dbPath, true, defaultDBTimeout, false,
	)
	if err != nil {
		db, err = walletdb.Create(
			"bdb", dbPath, true, defaultDBTimeout, false,
		)
		if err != nil {
			return nil, fmt.Errorf("create neutrino db: %w", err)
		}
	}

	blockCache := blockcache.NewBlockCache(defaultBlockCacheSize)

	cfg := neutrino.Config{
		DataDir:       dataDir,
		Database:      db,
		ChainParams:   *chainParams,
		ConnectPeers:  connectPeers,
		AddPeers:      addPeers,
		BlockCache:    blockCache.Cache,
		PersistToDisk: persistFilters,
		HeadersImport: headersImport,
	}

	cs, err := neutrino.NewChainService(cfg)
	if err != nil {
		_ = db.Close()

		return nil, fmt.Errorf("create neutrino service: %w", err)
	}

	svc := &NeutrinoService{
		cs:          cs,
		db:          db,
		blockCache:  blockCache,
		chainParams: chainParams,
		log:         logger,
		// Preserve existing behavior unless the caller explicitly opts
		// out for parallel in-process tests.
		wireGlobalLoggers: true,
	}
	for _, opt := range opts {
		opt(svc)
	}

	return svc, nil
}

// neutrinoHeadersImportConfig validates and builds the optional neutrino
// header import configuration used for fast initial sync.
func neutrinoHeadersImportConfig(blockHeadersSource,
	filterHeadersSource string) (*neutrino.HeadersImportConfig, error) {

	blockSet := blockHeadersSource != ""
	filterSet := filterHeadersSource != ""

	if blockSet != filterSet {
		return nil, fmt.Errorf("both block and filter header sources " +
			"must be specified together for headers import")
	}

	if !blockSet {
		return nil, nil
	}

	return &neutrino.HeadersImportConfig{
		BlockHeadersSource:  blockHeadersSource,
		FilterHeadersSource: filterHeadersSource,
		ValidationFlags:     neutrinoHeadersImportValidationFlags(),
	}, nil
}

// neutrinoHeadersImportValidationFlags returns validation flags for imported
// headers. The import path already validates network magic, header linkage, and
// header sanity. BFFastAdd keeps that behavior aligned with neutrino's normal
// headers-first sync path, where historical public-network headers may be added
// without replaying every contextual timestamp and difficulty check.
func neutrinoHeadersImportValidationFlags() blockchain.BehaviorFlags {
	return blockchain.BFFastAdd
}

// Start begins the neutrino chain service, connecting to peers and
// syncing headers and compact block filters.
func (n *NeutrinoService) Start(ctx context.Context) error {
	n.log.InfoS(ctx, "Starting neutrino chain service")

	if n.wireGlobalLoggers {
		// Neutrino and btcwallet only expose package-global logger
		// hooks. Enable them by default for normal daemon use so useful
		// peer, sync, rescan, and wallet internals remain visible.
		neutrino.UseLogger(n.log)
		chain.UseLogger(n.log)
		basewallet.UseLogger(n.log)
	}

	if err := n.cs.Start(ctx); err != nil {
		return fmt.Errorf("start neutrino: %w", err)
	}

	n.log.InfoS(ctx, "Neutrino chain service started")

	return nil
}

// Stop shuts down the neutrino chain service and closes the
// backing database.
func (n *NeutrinoService) Stop() error {
	ctx := context.Background()
	n.log.InfoS(ctx, "Stopping neutrino chain service")

	if err := n.cs.Stop(); err != nil {
		n.log.WarnS(ctx, "Error stopping neutrino", err)
	}

	if err := n.db.Close(); err != nil {
		return fmt.Errorf("close neutrino db: %w", err)
	}

	n.log.InfoS(ctx, "Neutrino chain service stopped")

	return nil
}

// ChainService returns the underlying neutrino.ChainService. The
// service must be started before calling this.
func (n *NeutrinoService) ChainService() *neutrino.ChainService {
	return n.cs
}

// BlockCache returns the shared block cache used by both neutrino
// and the NeutrinoNotifier.
func (n *NeutrinoService) BlockCache() *blockcache.BlockCache {
	return n.blockCache
}

// ChainClient creates a new btcwallet chain.NeutrinoClient that
// implements chain.Interface for use by btcwallet. Each call creates
// a fresh client instance.
func (n *NeutrinoService) ChainClient() *chain.NeutrinoClient {
	return chain.NewNeutrinoClient(n.chainParams, n.cs)
}

// BestBlock returns the current best block height and hash from
// neutrino's perspective.
func (n *NeutrinoService) BestBlock() (int32, error) {
	bs, err := n.cs.BestBlock()
	if err != nil {
		return 0, fmt.Errorf("neutrino best block: %w", err)
	}

	n.log.DebugS(context.Background(), "Neutrino best block",
		slog.Int("height", int(bs.Height)),
		slog.String("hash", bs.Hash.String()),
	)

	return bs.Height, nil
}
