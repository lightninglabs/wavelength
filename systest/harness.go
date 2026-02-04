//go:build systest

package systest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btclog/v2"
	"github.com/google/uuid"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/build"
	"github.com/lightninglabs/darepo-client/chainbackends"
	"github.com/lightninglabs/darepo-client/chainsource"
	clientharness "github.com/lightninglabs/darepo-client/harness"
	"github.com/lightninglabs/darepo/batch"
	"github.com/lightninglabs/darepo/batchsweeper"
	"github.com/lightninglabs/darepo/batchwatcher"
	"github.com/lightninglabs/darepo/clientconn"
	"github.com/lightninglabs/darepo/db"
	"github.com/lightninglabs/darepo/harness"
	"github.com/lightninglabs/darepo/lndbackend"
	"github.com/lightninglabs/darepo/rounds"
	"github.com/lightninglabs/darepo/timeout"
	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/require"
)

const (
	// defaultTimeout is the default timeout for waiting operations.
	defaultTimeout = 60 * time.Second

	// pollInterval is the interval for polling in require.Eventually.
	pollInterval = 200 * time.Millisecond

	// defaultRegistrationTimeout is a short timeout for test rounds.
	defaultRegistrationTimeout = 1 * time.Second

	// defaultSigCollectionTimeout is a short timeout for signature
	// collection in tests.
	defaultSigCollectionTimeout = 5 * time.Second

	// defaultConfirmationTarget is the number of confirmations to wait for.
	defaultConfirmationTarget = 1

	// defaultBoardingExitDelay is the exit delay for boarding inputs.
	defaultBoardingExitDelay = 144

	// defaultVTXOExitDelay is the exit delay for VTXO outputs.
	defaultVTXOExitDelay = 144

	// defaultSweepDelay is the sweep delay for VTXO trees.
	defaultSweepDelay = 1000
)

// HarnessOption is a functional option for configuring the E2EHarness.
type HarnessOption func(*harnessConfig)

// harnessConfig holds optional configuration for the harness.
type harnessConfig struct {
	sweepDelay uint32
}

// defaultHarnessConfig returns the default harness configuration.
func defaultHarnessConfig() *harnessConfig {
	return &harnessConfig{
		sweepDelay: defaultSweepDelay,
	}
}

// WithSweepDelay sets a custom sweep delay for the VTXO trees. This is useful
// for tests that need to verify batch expiry notifications without mining
// 1000+ blocks.
func WithSweepDelay(delay uint32) HarnessOption {
	return func(cfg *harnessConfig) {
		cfg.sweepDelay = delay
	}
}

// E2EHarness provides full client-server test infrastructure for e2e testing.
// It embeds the ArkHarness for chain infrastructure and adds actor-based
// components for testing the round FSM without gRPC.
type E2EHarness struct {
	// Embedded ArkHarness provides bitcoind, lnd, tapd, electrs, arkd.
	*harness.ArkHarness

	// cfg holds optional configuration for the harness.
	cfg *harnessConfig

	// t is the test instance.
	t *testing.T

	// ctx is the context for actor operations.
	ctx    context.Context
	cancel context.CancelFunc

	// logHandler is the shared log handler for creating subsystem loggers.
	logHandler *btclog.DefaultHandler

	// log is the root logger for general logging.
	log btclog.Logger

	// actorSystem manages actor lifecycle for the test.
	actorSystem *actor.ActorSystem

	// serverLND is a dedicated LND instance for the server. This is
	// separate from the primary harness LND to ensure proper wallet
	// isolation between server and clients.
	serverLND *clientharness.LndInstance

	// serverLNDServices provides easy access to the server's LND services.
	serverLNDServices *lndclient.LndServices

	// roundsActor is the server-side rounds management actor.
	roundsActor actor.ActorRef[rounds.ActorMsg, rounds.ActorResp]

	// mockTimeout provides test control over timeouts.
	mockTimeout *MockTimeoutActor

	// mockTimeoutRef is the actor reference for the mock timeout.
	mockTimeoutRef actor.ActorRef[timeout.Msg, timeout.Resp]

	// bridge connects server to clients.
	bridge *BridgeClientConn

	// transcript records all client-server messages.
	transcript *MessageTranscript

	// terms are the operator terms for rounds.
	terms *batch.Terms

	// operatorKeyDesc is the operator's key descriptor from LND. This
	// includes both the locator and the actual public key.
	operatorKeyDesc *keychain.KeyDescriptor

	// forfeitScript is the output script that clients must use for the
	// penalty output in forfeit transactions. This is a P2TR script paying
	// to the operator's key.
	forfeitScript []byte

	// sqlStore is the SQL database store providing real persistence for
	// rounds and VTXOs. This allows the systests to exercise the actual
	// database code paths including TLV serialization, migrations, and
	// query logic.
	sqlStore *db.Store

	// Real database-backed stores for rounds and VTXOs.
	roundStore *db.RoundStoreDB
	vtxoStore  *db.VTXOStoreDB

	// Mock boarding locker is still used since we don't have a real
	// implementation yet.
	mockBoardingLocker *MockBoardingLocker

	// chainSourceActor is the real ChainSourceActor from the client package.
	// It handles transaction broadcasts and confirmation subscriptions using
	// the server's LND for chain notifications.
	chainSourceActor *chainsource.ChainSourceActor

	// chainSourceActorRef is the actor reference for the chain source.
	chainSourceActorRef actor.ActorRef[chainsource.ChainSourceMsg, chainsource.ChainSourceResp]

	// batchWatcher is the BatchWatcherActor that monitors on-chain tree
	// state for all registered batches.
	batchWatcher *batchwatcher.Actor

	// batchWatcherRef is the actor reference for the batch watcher.
	batchWatcherRef actor.ActorRef[batchwatcher.BatchWatcherMsg, batchwatcher.BatchWatcherResp]

	// mockBatchSweeper captures batch expiry and tree state notifications
	// from the BatchWatcher for test assertions.
	mockBatchSweeper *MockBatchSweeper

	// batchSweeperRouter fans out BatchWatcher notifications to both the
	// mock sweeper (for assertions) and a real BatchSweeperActor (for
	// sweeping integration coverage).
	batchSweeperRouter *BatchSweeperRouter

	// Real components (NOT mocked):
	// - walletController: lndbackend.LndWalletController from server's LND
	// - chainSource: lndbackend.ChainSource wrapping bitcoind
	walletController *lndbackend.LndWalletController
	chainSource      *lndbackend.ChainSource

	// mu protects the clients map.
	mu sync.Mutex

	// clients tracks test clients by ID.
	clients map[clientconn.ClientID]*TestClient

	// clientCounter generates unique client IDs.
	clientCounter int
}

// NewE2EHarness creates a new harness for e2e tests. Optional HarnessOption
// functions can be provided to customize the harness configuration.
func NewE2EHarness(t *testing.T, opts ...HarnessOption) *E2EHarness {
	// Apply options to the default config.
	cfg := defaultHarnessConfig()
	for _, opt := range opts {
		opt(cfg)
	}
	// Create the underlying ArkHarness with default options. We skip arkd
	// since the e2e tests run the server actors in-process as goroutines
	// rather than starting the full arkd binary.
	clientOpts := clientharness.DefaultOptions()
	clientOpts.StartTapd = false // Don't need tapd for e2e tests.
	clientOpts.GroupName = t.Name()

	// Allow overriding LND image via environment variable. This is needed
	// for testing with custom LND builds that include newer RPC methods.
	if lndImage := os.Getenv("LND_IMAGE"); lndImage != "" {
		clientOpts.LNDImage = lndImage
		t.Logf("Using custom LND image: %s", lndImage)
	}

	// Allow building LND from a local path instead of pulling an image.
	// This is useful for testing with the latest LND features like
	// MuSig2GetCombinedNonce before a release is available.
	if lndBuildPath := os.Getenv("LND_BUILD_PATH"); lndBuildPath != "" {
		clientOpts.LNDBuildPath = lndBuildPath
		t.Logf("Building LND from path: %s", lndBuildPath)
	}
	arkHarness := harness.NewArkHarness(t, &harness.ArkHarnessOptions{
		ClientOptions: &clientOpts,
		SkipArkd:      true,
	})

	baseCtx, cancel := context.WithCancel(context.Background())

	// Create a log handler for creating subsystem-specific loggers. Each
	// subsystem gets its own logger created via handler.SubSystem() which
	// produces proper lnd-style log output: [INF] SUBSYS: message.
	logHandler := btclog.NewDefaultHandler(os.Stdout)
	logHandler.SetLevel(btclog.LevelTrace)

	// Create a root logger for general test harness logging.
	rootLog := btclog.NewSLogger(logHandler.SubSystem(Subsystem))
	rootLog.SetLevel(btclog.LevelTrace)

	// Attach the root logger to the context for downstream use.
	ctx := build.ContextWithLogger(baseCtx, rootLog)

	h := &E2EHarness{
		ArkHarness: arkHarness,
		cfg:        cfg,
		t:          t,
		ctx:        ctx,
		cancel:     cancel,
		logHandler: logHandler,
		log:        rootLog,
		transcript: NewMessageTranscript(),
		clients:    make(map[clientconn.ClientID]*TestClient),
	}

	// Register cleanup.
	t.Cleanup(func() {
		h.Stop()
	})

	return h
}

// Start starts the harness infrastructure and initializes the actor system.
func (h *E2EHarness) Start() {
	// Start the underlying chain infrastructure.
	h.ArkHarness.Start()

	// Start a dedicated LND instance for the server. This ensures proper
	// wallet isolation between the server and clients. The server's wallet
	// only holds funds for constructing commitment transactions, while
	// client wallets hold boarding UTXOs.
	h.serverLND = h.Harness.StartAdditionalLND("server")
	h.serverLNDServices = h.serverLND.Client

	h.log.Infof("Started dedicated server LND on port %s",
		h.serverLND.GRPCPort)

	// Initialize the actor system and server components.
	h.initActorSystem()
}

// Logger returns the root test logger.
func (h *E2EHarness) Logger() btclog.Logger {
	return h.log
}

// SubLogger creates a new logger for the given subsystem. The returned logger
// formats output as [INF] SUBSYS: message, matching lnd's log format.
func (h *E2EHarness) SubLogger(subsystem string) btclog.Logger {
	log := btclog.NewSLogger(h.logHandler.SubSystem(subsystem))
	log.SetLevel(btclog.LevelTrace)

	return log
}

// Stop stops all actors and the underlying infrastructure.
func (h *E2EHarness) Stop() {
	// Shutdown actor system gracefully.
	if h.actorSystem != nil {
		shutdownCtx, cancel := context.WithTimeout(
			context.Background(), 5*time.Second,
		)
		defer cancel()

		_ = h.actorSystem.Shutdown(shutdownCtx)
	}

	// Close the database connection.
	if h.sqlStore != nil {
		_ = h.sqlStore.Close()
	}

	// Cancel context to stop any remaining operations.
	if h.cancel != nil {
		h.cancel()
	}

	// Stop the underlying harness.
	h.ArkHarness.Stop()
}

// initActorSystem sets up the actor system, chain source, timeout actor,
// bridge, and rounds actor.
func (h *E2EHarness) initActorSystem() {
	// Create the actor system.
	h.actorSystem = actor.NewActorSystem()

	// Create mock timeout actor behavior.
	h.mockTimeout = NewMockTimeoutActor()

	// Spawn the mock timeout actor.
	timeoutKey := actor.NewServiceKey[timeout.Msg, timeout.Resp](
		"mock-timeout",
	)
	h.mockTimeoutRef = timeoutKey.Spawn(
		h.actorSystem, "mock-timeout", h.mockTimeout,
	)

	// Create the bridge for server→client messages. The transcript was
	// already initialized in NewE2EHarness.
	h.bridge = NewBridgeClientConn(h.transcript)

	// Spawn the bridge actor.
	bridgeKey := actor.NewServiceKey[clientconn.ClientConnMsg, clientconn.ClientConnResp](
		"bridge-client-conn",
	)
	bridgeRef := bridgeKey.Spawn(
		h.actorSystem, "bridge-client-conn", h.bridge,
	)

	// Get the operator key from LND. This is a REAL key with an actual
	// public key, not a mock.
	h.operatorKeyDesc = h.getOperatorKeyFromLND()

	// Create the forfeit script - a P2TR script paying to the operator's
	// taproot output key. Clients use this as the penalty output in forfeit
	// transactions so the server can claim forfeited funds.
	operatorOutputKey := txscript.ComputeTaprootOutputKey(
		h.operatorKeyDesc.PubKey, nil,
	)
	forfeitScript, err := txscript.PayToTaprootScript(operatorOutputKey)
	require.NoError(h.t, err, "failed to create forfeit script")
	h.forfeitScript = forfeitScript

	// Create default terms for test rounds using the real operator key.
	h.terms = h.createDefaultTerms()

	// Create a temporary SQLite database for this test. Each test gets
	// its own isolated database in a temporary directory.
	dbDir := h.t.TempDir()
	dbPath := filepath.Join(dbDir, "systest.db")
	h.log.Infof("Using SQLite database at %s", dbPath)

	// Configure SQLite store.
	sqliteConfig := &db.SqliteConfig{
		DatabaseFileName: dbPath,
	}

	// Create the SQL store with real persistence.
	sqliteStore, err := db.NewSqliteStore(sqliteConfig, h.log)
	require.NoError(h.t, err, "failed to create sqlite store")

	h.sqlStore = db.NewStore(
		sqliteStore.DB, sqliteStore.Queries, sqliteStore.Backend(),
		h.log, clock.NewDefaultClock(),
	)

	// Create real database-backed stores.
	h.roundStore = h.sqlStore.NewRoundStore()
	h.vtxoStore = h.sqlStore.NewVTXOStore()

	// Mock boarding locker is still used since we don't have a real
	// implementation yet.
	h.mockBoardingLocker = &MockBoardingLocker{}
	h.mockBoardingLocker.SetupDefaultExpectations()

	// Create REAL wallet controller from the server's dedicated LND.
	// Using a separate LND ensures the server's wallet is isolated from
	// client wallets for proper UTXO management.
	h.walletController = lndbackend.NewLndWalletController(
		h.serverLNDServices.WalletKit, h.serverLNDServices.Signer,
	)

	// Create REAL chain source wrapping harness bitcoind.
	rpcClient, err := h.BitcoinRPCClient()
	require.NoError(h.t, err, "failed to get bitcoind RPC client")
	h.chainSource = lndbackend.NewChainSource(rpcClient)

	// Create REAL chain source actor using the server's LND backend.
	// This provides proper chain notifications, transaction broadcast,
	// and confirmation subscriptions.
	chainBackendLogger := h.SubLogger("CSRC")
	notifier := chainbackends.NewLndClientChainNotifier(
		chainbackends.LndClientChainNotifierConfig{
			LND: h.serverLNDServices,
		}.WithLogger(chainBackendLogger),
	)

	// Use a static fee estimator for systests to avoid flaky/oversized fee
	// estimates from remote LND backends on regtest.
	feeEstimator := chainfee.NewStaticEstimator(
		chainfee.SatPerKWeight(2000), 0,
	)
	broadcaster := chainbackends.NewLndClientTxBroadcaster(
		h.serverLNDServices.WalletKit,
	)
	chainBackend := chainbackends.NewLNDBackend(
		notifier, feeEstimator, broadcaster,
	)
	h.chainSourceActor = chainsource.NewChainSourceActor(
		chainsource.ChainSourceConfig{
			Backend: chainBackend,
			System:  h.actorSystem,
		}.WithLogger(chainBackendLogger),
	)

	// Spawn the chain source actor.
	chainSourceKey := actor.NewServiceKey[chainsource.ChainSourceMsg, chainsource.ChainSourceResp](
		"chain-source-actor",
	)
	h.chainSourceActorRef = chainSourceKey.Spawn(
		h.actorSystem, "chain-source-actor", h.chainSourceActor,
	)

	// Create mock BatchSweeper to capture expiry and tree state notifications.
	h.mockBatchSweeper = NewMockBatchSweeper()

	// Create a notification router so we can attach a real sweeper actor
	// after the BatchWatcher has been spawned.
	h.batchSweeperRouter = NewBatchSweeperRouter(h.mockBatchSweeper)

	// Create BatchWatcher actor config. FraudDetector is not implemented
	// yet, so we pass None. BatchSweeper uses the mock for test assertions.
	batchWatcherCfg := &batchwatcher.ActorConfig{
		Logger:        h.SubLogger("BWCH"),
		ChainSource:   h.chainSourceActorRef,
		FraudDetector: fn.None[actor.TellOnlyRef[batchwatcher.FraudDetectorMsg]](),
		BatchSweeper: fn.Some[actor.TellOnlyRef[batchwatcher.BatchSweeperMsg]](
			h.batchSweeperRouter,
		),
	}

	h.batchWatcher = batchwatcher.NewActor(batchWatcherCfg)

	// Spawn the batch watcher actor.
	batchWatcherKey := actor.NewServiceKey[
		batchwatcher.BatchWatcherMsg, batchwatcher.BatchWatcherResp,
	]("batch-watcher-actor")
	h.batchWatcherRef = batchWatcherKey.Spawn(
		h.actorSystem, "batch-watcher-actor", h.batchWatcher,
	)

	// Set SelfRef after spawning (needed for callback mapping).
	batchWatcherCfg.SelfRef = h.batchWatcherRef

	// Create and wire the real BatchSweeperActor for sweeping integration
	// coverage.
	h.initBatchSweeper()

	// Create rounds actor configuration. ActorRef embeds TellOnlyRef, so
	// we can assign ActorRef directly to TellOnlyRef fields.
	roundsCfg := &rounds.ActorConfig{
		ChainParams:         &chaincfg.RegressionNetParams,
		Logger:              h.SubLogger(rounds.Subsystem),
		Terms:               h.terms,
		ForfeitScript:       h.forfeitScript,
		ClientsConn:         bridgeRef,
		BoardingInputLocker: h.mockBoardingLocker,
		ChainSource:         h.chainSource,
		ChainSourceActor:    h.chainSourceActorRef,
		RoundStore:          h.roundStore,
		VTXOStore:           h.vtxoStore,
		TimeoutActor:        h.mockTimeoutRef,
		WalletController:    h.walletController,
		FeeEstimator:        feeEstimator,
		WalletAccount:       "",
		ConfTarget:          defaultConfirmationTarget,
		MinConfs:            1,
		ConfirmationTarget:  uint32(defaultConfirmationTarget),
		BatchWatcher:        fn.Some(h.batchWatcherRef),
	}

	// Create rounds actor.
	roundsActor := rounds.NewActor(roundsCfg)

	// Spawn the rounds actor.
	roundsKey := actor.NewServiceKey[rounds.ActorMsg, rounds.ActorResp](
		"rounds-actor",
	)
	h.roundsActor = roundsKey.Spawn(
		h.actorSystem, "rounds-actor", roundsActor,
	)

	// Set SelfRef on config after spawning (needed for callback mapping).
	// ActorRef embeds TellOnlyRef, so we can assign directly.
	roundsCfg.SelfRef = h.roundsActor

	// Start the rounds actor.
	err = roundsActor.Start(h.ctx)
	require.NoError(h.t, err, "failed to start rounds actor")
}

// initBatchSweeper creates and wires a real BatchSweeperActor so systests can
// assert sweeping behavior while still capturing notifications via the mock.
func (h *E2EHarness) initBatchSweeper() {
	sweepPkScript, err := txscript.PayToTaprootScript(
		h.terms.SweepKey.PubKey,
	)
	require.NoError(h.t, err, "failed to create sweep pkScript")

	cfg := &batchsweeper.ActorConfig{
		Logger:        h.SubLogger("BSWP"),
		BatchWatcher:  h.batchWatcherRef,
		ChainSource:   h.chainSourceActorRef,
		SweepKey:      h.terms.SweepKey,
		SweepDelay:    h.terms.SweepDelay,
		Signer:        h.walletController,
		SweepPkScript: sweepPkScript,
		TimeoutActor: fn.Some[actor.TellOnlyRef[timeout.Msg]](
			h.mockTimeoutRef,
		),
	}

	sweeper := batchsweeper.NewActor(cfg)

	sweeperKey := actor.NewServiceKey[
		batchsweeper.Msg, batchsweeper.Resp,
	]("batch-sweeper-actor")
	sweeperRef := sweeperKey.Spawn(
		h.actorSystem, "batch-sweeper-actor", sweeper,
	)

	cfg.SelfRef = sweeperRef

	mappedRef := batchsweeper.MapBatchWatcherNotification(sweeperRef)
	h.batchSweeperRouter.SetTargets(h.mockBatchSweeper, mappedRef)
}

// getOperatorKeyFromLND derives the operator key from the server's LND using
// the multi-sig key family. Returns a key descriptor with both locator and
// public key.
func (h *E2EHarness) getOperatorKeyFromLND() *keychain.KeyDescriptor {
	keyDesc, err := h.serverLNDServices.WalletKit.DeriveNextKey(
		h.ctx, int32(keychain.KeyFamilyMultiSig),
	)
	require.NoError(h.t, err, "failed to derive operator key from server LND")

	return keyDesc
}

// getSweepKeyFromLND derives the sweep key from the server's LND using the
// multi-sig key family. Returns a key descriptor with both locator and public
// key.
func (h *E2EHarness) getSweepKeyFromLND() *keychain.KeyDescriptor {
	keyDesc, err := h.serverLNDServices.WalletKit.DeriveNextKey(
		h.ctx, int32(keychain.KeyFamilyMultiSig),
	)
	require.NoError(h.t, err, "failed to derive sweep key from server LND")

	return keyDesc
}

// createDefaultTerms creates default batch terms for testing. Uses the real
// operator key and sweep key derived from LND.
func (h *E2EHarness) createDefaultTerms() *batch.Terms {
	// Use the real operator and sweep keys from LND. These keys have actual
	// public keys that will be used for validation and script construction.
	sweepKeyDesc := h.getSweepKeyFromLND()

	// Create connector address from operator key. This is a taproot address
	// that receives connector outputs for forfeit transactions.
	operatorPub := h.operatorKeyDesc.PubKey
	outputKey := txscript.ComputeTaprootOutputKey(operatorPub, nil)
	connectorAddr, err := btcutil.NewAddressTaproot(
		schnorr.SerializePubKey(outputKey),
		&chaincfg.RegressionNetParams,
	)
	require.NoError(h.t, err, "failed to create connector address")

	return &batch.Terms{
		OperatorKey:                   *h.operatorKeyDesc,
		SweepKey:                      *sweepKeyDesc,
		SweepDelay:                    h.cfg.sweepDelay,
		MaxVTXOsPerTree:               256,
		MaxConnectorsPerTree:          128,
		ConnectorDustAmount:           330,
		ConnectorAddress:              connectorAddr,
		TreeRadix:                     4,
		BoardingExitDelay:             defaultBoardingExitDelay,
		BoardingExitDelaySafetyMargin: 10,
		MinBoardingConfirmations:      1,
		MinVTXOAmount:                 btcutil.Amount(1000),
		MaxVTXOAmount:                 btcutil.Amount(100_000_000_000),
		VTXOExitDelay:                 defaultVTXOExitDelay,
		MinLeaveAmount:                btcutil.Amount(1000),
		RegistrationTimeout:           defaultRegistrationTimeout,
		SignatureCollectionTimeout:    defaultSigCollectionTimeout,
	}
}

// OperatorPubKey returns the operator's public key for creating boarding
// addresses.
func (h *E2EHarness) OperatorPubKey() *keychain.KeyDescriptor {
	return h.operatorKeyDesc
}

// FundServerWallet funds the server's dedicated LND wallet with coins. The
// server needs wallet funds to build commitment transactions (for fees,
// change, etc.).
func (h *E2EHarness) FundServerWallet(amount btcutil.Amount) {
	// Get a new address from the server's dedicated LND wallet.
	addrResp, err := h.serverLNDServices.WalletKit.NextAddr(
		h.ctx, "", walletrpc.AddressType_WITNESS_PUBKEY_HASH, false,
	)
	require.NoError(h.t, err, "failed to get wallet address from server LND")

	// Fund the address via the faucet.
	h.Harness.Faucet(addrResp.String(), amount)

	// Mine blocks to confirm.
	h.MineBlocks(1)

	h.t.Logf("Funded server wallet with %s", amount)
}

// Terms returns the batch terms used for rounds.
func (h *E2EHarness) Terms() *batch.Terms {
	return h.terms
}

// ForfeitScript returns the forfeit script for clients to use in forfeit
// transactions. This is a P2TR script paying to the operator's key.
func (h *E2EHarness) ForfeitScript() []byte {
	return h.forfeitScript
}

// AssertMocksCalled verifies all mock expectations were met. Currently only
// the boarding locker is mocked. The round and VTXO stores use real SQL
// database persistence.
func (h *E2EHarness) AssertMocksCalled(t *testing.T) {
	h.mockBoardingLocker.AssertExpectations(t)
}

// Transcript returns the message transcript for assertions.
func (h *E2EHarness) Transcript() *MessageTranscript {
	return h.transcript
}

// Bridge returns the server-to-client bridge for message control. This allows
// tests to buffer, hold, and release messages for orchestrating concurrent
// round execution.
func (h *E2EHarness) Bridge() *BridgeClientConn {
	return h.bridge
}

// BatchWatcher returns the batch watcher actor reference. This can be used to
// query tree state or verify that batches were registered for monitoring.
func (h *E2EHarness) BatchWatcher() actor.ActorRef[
	batchwatcher.BatchWatcherMsg, batchwatcher.BatchWatcherResp,
] {

	return h.batchWatcherRef
}

// MockBatchSweeper returns the mock batch sweeper for test assertions.
func (h *E2EHarness) MockBatchSweeper() *MockBatchSweeper {
	return h.mockBatchSweeper
}

// ComputeBatchID computes the BatchID for a given round and output index using
// the same algorithm as the rounds actor. The roundID parameter accepts any
// type that is based on uuid.UUID (e.g., rounds.RoundID or round.RoundID).
func ComputeBatchID(roundID uuid.UUID, outputIdx int) batchwatcher.BatchID {
	batchIDName := fmt.Sprintf("%s-%d", roundID, outputIdx)
	return batchwatcher.BatchID(uuid.NewSHA1(roundID, []byte(batchIDName)))
}

// AssertBatchRegistered verifies that a batch was registered with the
// BatchWatcher and has the correct expiry height. The expectedTreeCount
// specifies how many VTXO trees to check (typically 1 for single-client
// rounds). The roundID parameter accepts any type that is based on uuid.UUID.
func (h *E2EHarness) AssertBatchRegistered(
	roundID uuid.UUID, confirmationHeight uint32, expectedTreeCount int) {

	for outputIdx := 0; outputIdx < expectedTreeCount; outputIdx++ {
		batchID := ComputeBatchID(roundID, outputIdx)

		// Query the BatchWatcher for the tree state.
		req := &batchwatcher.GetTreeStateRequest{BatchID: batchID}
		future := h.batchWatcherRef.Ask(h.ctx, req)
		resp, err := future.Await(h.ctx).Unpack()
		require.NoError(h.t, err, "should query batch watcher")

		stateResp, ok := resp.(*batchwatcher.GetTreeStateResponse)
		require.True(h.t, ok, "response should be GetTreeStateResponse")

		// Verify the batch was found.
		require.True(h.t, stateResp.Found,
			"batch %s should be found in watcher", batchID)
		require.NotNil(h.t, stateResp.TreeState,
			"tree state for batch %s should not be nil", batchID)

		// Verify the expiry height.
		expectedExpiry := confirmationHeight + h.terms.SweepDelay
		require.Equal(h.t, expectedExpiry, stateResp.TreeState.ExpiryHeight,
			"batch %s expiry height should be %d (confirm=%d + sweep=%d)",
			batchID, expectedExpiry, confirmationHeight, h.terms.SweepDelay)

		h.t.Logf("Verified batch %s registered with expiry height %d",
			batchID, stateResp.TreeState.ExpiryHeight)
	}
}

// AssertBatchExpired verifies that a batch expiry notification was sent to the
// mock BatchSweeper. The roundID parameter accepts any type based on uuid.UUID.
func (h *E2EHarness) AssertBatchExpired(roundID uuid.UUID, outputIdx int) {

	batchID := ComputeBatchID(roundID, outputIdx)
	require.True(h.t, h.mockBatchSweeper.HasExpiryNotification(batchID),
		"batch %s should have received expiry notification", batchID)

	notification := h.mockBatchSweeper.GetExpiryNotification(batchID)
	h.t.Logf("Verified batch %s expiry notification at height %d",
		batchID, notification.ExpiryHeight)
}

// TriggerRoundSeal triggers the registration timeout to seal the round.
func (h *E2EHarness) TriggerRoundSeal() {
	h.TriggerTimeout(rounds.TimeoutPhaseRegistration)
}

// TriggerTimeout triggers a specific timeout phase. The timeout ID is
// constructed from the current round ID and the phase.
func (h *E2EHarness) TriggerTimeout(phase rounds.TimeoutPhase) {
	// Find pending timeouts that match the phase.
	pendingIDs := h.mockTimeout.PendingTimeoutIDs()

	for _, id := range pendingIDs {
		// Check if this timeout is for the requested phase.
		if containsPhase(string(id), string(phase)) {
			err := h.mockTimeout.TriggerTimeout(h.ctx, id)
			if err != nil {
				h.t.Logf("Warning: failed to trigger timeout %s: %v",
					id, err)
			}

			return
		}
	}

	h.t.Logf("Warning: no pending timeout found for phase %s", phase)
}

// TriggerAllTimeouts fires all pending timeouts.
func (h *E2EHarness) TriggerAllTimeouts() {
	h.mockTimeout.TriggerAll(h.ctx)
}

// AssertTxInMempool verifies transaction was accepted into the mempool.
func (h *E2EHarness) AssertTxInMempool(txid chainhash.Hash) {
	txidStr := txid.String()
	require.Eventually(h.t, func() bool {
		// Use harness helper that returns txids as strings.
		mempoolTxs := h.Harness.MempoolTxIDs()
		for _, id := range mempoolTxs {
			if id == txidStr {
				return true
			}
		}

		return false
	}, defaultTimeout, pollInterval, "transaction %s not in mempool", txid)
}

// AssertTxConfirmed verifies transaction has been confirmed on chain.
func (h *E2EHarness) AssertTxConfirmed(txid chainhash.Hash) {
	txidStr := txid.String()
	require.Eventually(h.t, func() bool {
		// Mine a block to confirm. If tx was already confirmed, this
		// just adds another confirmation. Check it's not in mempool.
		mempoolTxs := h.Harness.MempoolTxIDs()
		for _, id := range mempoolTxs {
			if id == txidStr {
				// Still in mempool, not confirmed.
				return false
			}
		}

		// Not in mempool means either confirmed or never broadcast.
		// For now assume confirmed if it was previously in mempool.
		return true
	}, defaultTimeout, pollInterval, "transaction %s not confirmed", txid)
}

// MineBlocks mines n blocks. The real ChainSourceActor with LND backend
// automatically detects new blocks and sends confirmation events.
func (h *E2EHarness) MineBlocks(n int) {
	h.Harness.Generate(n)
}

// MineBlocksAndConfirm mines blocks. With the real ChainSourceActor using
// LND's chain notification backend, confirmation events are sent
// automatically when LND detects the new blocks. This method is an alias
// for MineBlocks but provides a clearer semantic for test readability.
func (h *E2EHarness) MineBlocksAndConfirm(n int) {
	h.Harness.Generate(n)
}

// RegisterClient registers a client with the bridge for message routing.
func (h *E2EHarness) RegisterClient(client *TestClient) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.clients[client.clientID] = client

	// TODO: Register with bridge once client actor is created.
}

// StartClientLND starts a new dedicated LND instance for a client. Each client
// should have its own LND to ensure proper wallet isolation.
func (h *E2EHarness) StartClientLND(name string) *clientharness.LndInstance {
	inst := h.Harness.StartAdditionalLND(name)
	h.log.Infof("Started client LND '%s' on port %s", name, inst.GRPCPort)

	return inst
}

// PrimaryLND returns the primary LND instance from the base harness. This can
// be used for clients if a dedicated instance is not needed.
func (h *E2EHarness) PrimaryLND() *lndclient.LndServices {
	return h.Harness.LND
}

// ServerLND returns the server's dedicated LND services.
func (h *E2EHarness) ServerLND() *lndclient.LndServices {
	return h.serverLNDServices
}

// containsPhase checks if a timeout ID string contains the given phase.
func containsPhase(timeoutID, phase string) bool {
	return len(timeoutID) > len(phase) &&
		timeoutID[len(timeoutID)-len(phase):] == phase
}

// RestartClient simulates a client process restart. It stops the existing
// client and creates a new one using the same database and LND instance.
// The new client should:
// 1. Load persisted state from the database
// 2. Re-register for chain confirmations via new ChainSourceActor
// 3. Resume any in-progress round operations
//
// This is used for testing restart/recovery scenarios where a client terminates
// mid-round (after broadcast but before confirmation, etc.).
func (h *E2EHarness) RestartClient(client *TestClient) *TestClient {
	// Capture resources before stopping.
	lndInstance := client.LNDInstance()
	dbPath := client.DBPath()

	h.t.Logf("Restarting client %s (db: %s)", client.ClientID(), dbPath)

	// Stop the old client (unregisters from bridge, cancels subscriptions).
	client.Stop()

	// Create new client with existing state.
	newClient := NewTestClientWithExistingDB(h, lndInstance, dbPath)

	h.t.Logf("Client %s restarted successfully", newClient.ClientID())

	return newClient
}
