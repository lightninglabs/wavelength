//go:build systest

// Package systest provides end-to-end system testing infrastructure for the
// boarding wallet and related actors. Tests use real LND nodes on regtest via
// Docker, with per-test isolation of actor systems and databases.
package systest

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/build"
	"github.com/lightninglabs/darepo-client/chainbackends"
	"github.com/lightninglabs/darepo-client/chainsource"
	"github.com/lightninglabs/darepo-client/db"
	"github.com/lightninglabs/darepo-client/harness"
	"github.com/lightninglabs/darepo-client/lndbackend"
	"github.com/lightninglabs/darepo-client/wallet"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/stretchr/testify/require"
)

// SysTestHarness wraps harness.Harness with per-test actor system and database.
// Each test gets its own isolated Docker infrastructure, ActorSystem, and
// database. Use ParallelN(t) to run tests in parallel with controlled
// concurrency.
type SysTestHarness struct {
	// Harness is the per-test Docker harness.
	Harness *harness.Harness

	t *testing.T

	//nolint:containedctx
	ctx         context.Context
	cancel      context.CancelFunc
	actorSystem *actor.ActorSystem
	store       *db.BoardingWalletStore
	chainParams *chaincfg.Params

	// logHandler is the shared log handler for creating subsystem loggers.
	logHandler *btclog.DefaultHandler

	// log is the root logger for general logging.
	log btclog.Logger

	// cleanup holds functions to run on Close.
	cleanup []func()
}

// NewSysTestHarness creates a new test harness for a specific test. Each test
// gets its own Docker infrastructure (bitcoind, lnd), actor system, and
// database for full isolation. Call ParallelN(t) before this to enable
// parallel execution with controlled concurrency.
func NewSysTestHarness(t *testing.T) *SysTestHarness {
	t.Helper()

	// Create per-test Docker harness. We don't need tapd for boarding.
	opts := harness.DefaultOptions()
	opts.StartTapd = false
	opts.GroupName = t.Name()

	dockerHarness := harness.NewHarness(t, &opts)
	t.Cleanup(dockerHarness.Stop)
	dockerHarness.Start()

	baseCtx, cancel := context.WithCancel(t.Context())

	// Create a log handler for creating subsystem-specific loggers.
	// Each subsystem gets its own logger created via handler.SubSystem()
	// which produces proper lnd-style log output: [INF] SUBSYS: message.
	logHandler := btclog.NewDefaultHandler(os.Stdout)
	logHandler.SetLevel(btclog.LevelInfo)

	// Create a root logger for general test harness logging.
	rootLog := btclog.NewSLogger(logHandler.SubSystem(Subsystem))
	rootLog.SetLevel(btclog.LevelInfo)

	// Attach the root logger to the context for downstream use.
	ctx := build.ContextWithLogger(baseCtx, rootLog)

	// Create per-test actor system.
	actorSystem := actor.NewActorSystem()

	// Create per-test in-memory SQLite database.
	sqlDB := db.NewTestDB(t)

	// Create the transaction executor for the boarding store. We use the
	// sqlDB directly since it implements DatabaseBackend which extends
	// BatchedQuerier.
	boardingDB := db.NewTransactionExecutor(
		sqlDB,
		func(tx *sql.Tx) db.BoardingStore {
			return sqlDB.WithTx(tx)
		},
		rootLog,
	)

	// The pending-intent outbox shares the same database; the second
	// executor exists only because the generic transaction executor is
	// typed per query-interface.
	intentDB := db.NewTransactionExecutor(
		sqlDB,
		func(tx *sql.Tx) db.PendingIntentStore {
			return sqlDB.WithTx(tx)
		},
		rootLog,
	)

	// Create the boarding wallet store with the transaction executor.
	store := db.NewBoardingWalletStore(
		boardingDB, intentDB, &chaincfg.RegressionNetParams,
		clock.NewDefaultClock(),
	)

	sth := &SysTestHarness{
		Harness:     dockerHarness,
		t:           t,
		ctx:         ctx,
		cancel:      cancel,
		actorSystem: actorSystem,
		store:       store,
		chainParams: &chaincfg.RegressionNetParams,
		logHandler:  logHandler,
		log:         rootLog,
	}

	// Register cleanup.
	t.Cleanup(func() {
		sth.Close()
	})

	return sth
}

// Context returns the test context.
func (h *SysTestHarness) Context() context.Context {
	return h.ctx
}

// ActorSystem returns the per-test actor system.
func (h *SysTestHarness) ActorSystem() *actor.ActorSystem {
	return h.actorSystem
}

// BoardingStore returns the per-test boarding store.
func (h *SysTestHarness) BoardingStore() wallet.BoardingStore {
	return h.store
}

// ChainParams returns the chain parameters (regtest).
func (h *SysTestHarness) ChainParams() *chaincfg.Params {
	return h.chainParams
}

// Logger returns the test logger.
func (h *SysTestHarness) Logger() btclog.Logger {
	return h.log
}

// SubLogger creates a new logger for the given subsystem. The returned logger
// formats output as [INF] SUBSYS: message, matching lnd's log format.
func (h *SysTestHarness) SubLogger(subsystem string) btclog.Logger {
	log := btclog.NewSLogger(h.logHandler.SubSystem(subsystem))
	log.SetLevel(btclog.LevelInfo)

	return log
}

// NewBoardingBackend creates a new BoardingBackend connected to the shared LND.
func (h *SysTestHarness) NewBoardingBackend() wallet.BoardingBackend {
	return lndbackend.NewBoardingBackend(
		h.Harness.LND.WalletKit, h.Harness.LND.ChainKit,
	)
}

// NewChainBackend creates a new ChainBackend connected to the shared LND.
// The subsystem logger is passed via config to enable proper logging.
func (h *SysTestHarness) NewChainBackend() chainsource.ChainBackend {
	backend, err := chainbackends.NewLNDBackendFromLndClient(
		chainbackends.LNDBackendFromLndClientConfig{
			LND: h.Harness.LND,
		}.WithLogger(
			h.SubLogger(chainbackends.LndClientSubsystem),
		),
	)
	require.NoError(h.t, err)

	return backend
}

// NewChainSourceActor creates and spawns a ChainSourceActor using the shared
// LND connection and per-test actor system.
func (h *SysTestHarness) NewChainSourceActor() actor.ActorRef[
	chainsource.ChainSourceMsg, chainsource.ChainSourceResp] {

	backend := h.NewChainBackend()
	require.NoError(h.t, backend.Start())

	h.cleanup = append(h.cleanup, func() {
		_ = backend.Stop()
	})

	// Pass the backend, system, and logger via config. The actor and its
	// spawned sub-actors will use subsystem-specific loggers.
	chainSourceActor := chainsource.NewChainSourceActor(
		chainsource.ChainSourceConfig{
			Backend: backend,
			System:  h.actorSystem,
		}.WithLogger(
			h.SubLogger(chainsource.Subsystem),
		),
	)

	chainSourceRef := actor.RegisterWithSystem(
		h.actorSystem, "chain-source", actor.NewServiceKey[
			chainsource.ChainSourceMsg, chainsource.ChainSourceResp,
		](
			"chain-source",
		),
		chainSourceActor,
	)

	return chainSourceRef
}

// Close cleans up the per-test resources.
func (h *SysTestHarness) Close() {
	// Run cleanup functions in reverse order.
	for i := len(h.cleanup) - 1; i >= 0; i-- {
		h.cleanup[i]()
	}

	h.cancel()

	// Shutdown actor system with timeout.
	shutdownCtx, cancel := context.WithTimeout(
		context.Background(), DefaultTimeout,
	)
	defer cancel()

	_ = h.actorSystem.Shutdown(shutdownCtx)
}

// WaitForLNDSync waits for the shared LND to sync to chain after mining.
func (h *SysTestHarness) WaitForLNDSync() {
	h.Harness.WaitForLNDChainSync()
}

const (
	// DefaultTimeout is the default timeout for various operations.
	DefaultTimeout = 30 * time.Second
)
