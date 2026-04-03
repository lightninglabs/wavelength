package btcwbackend

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
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
}

// NewNeutrinoService creates a new neutrino chain service from the
// given configuration. The service is NOT started — call Start()
// after construction.
func NewNeutrinoService(dataDir string, chainParams *chaincfg.Params,
	connectPeers, addPeers []string, persistFilters bool,
	logger btclog.Logger) (*NeutrinoService, error) {

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
			"bdb", dbPath, true, defaultDBTimeout,
			false,
		)
		if err != nil {
			return nil, fmt.Errorf(
				"create neutrino db: %w", err,
			)
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
	}

	cs, err := neutrino.NewChainService(cfg)
	if err != nil {
		_ = db.Close()

		return nil, fmt.Errorf("create neutrino service: %w", err)
	}

	return &NeutrinoService{
		cs:          cs,
		db:          db,
		blockCache:  blockCache,
		chainParams: chainParams,
		log:         logger,
	}, nil
}

// Start begins the neutrino chain service, connecting to peers and
// syncing headers and compact block filters.
func (n *NeutrinoService) Start() error {
	n.log.InfoS(context.Background(), "Starting neutrino chain service")

	// Wire up neutrino and btcwallet chain client internal loggers
	// so their debug output is visible alongside our daemon logs.
	neutrino.UseLogger(n.log)
	chain.UseLogger(n.log)
	basewallet.UseLogger(n.log)

	if err := n.cs.Start(); err != nil {
		return fmt.Errorf("start neutrino: %w", err)
	}

	n.log.InfoS(
		context.Background(), "Neutrino chain service started",
	)

	return nil
}

// Stop shuts down the neutrino chain service and closes the
// backing database.
func (n *NeutrinoService) Stop() error {
	n.log.InfoS(context.Background(), "Stopping neutrino chain service")

	if err := n.cs.Stop(); err != nil {
		n.log.WarnS(
			context.Background(),
			"Error stopping neutrino", err,
		)
	}

	if err := n.db.Close(); err != nil {
		return fmt.Errorf("close neutrino db: %w", err)
	}

	n.log.InfoS(
		context.Background(), "Neutrino chain service stopped",
	)

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
		slog.String("hash", bs.Hash.String()))

	return bs.Height, nil
}
