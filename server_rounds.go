package darepo

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo/batch"
	"github.com/lightninglabs/darepo/batchwatcher"
	"github.com/lightninglabs/darepo/clientconn"
	"github.com/lightninglabs/darepo/db"
	"github.com/lightninglabs/darepo/lndbackend"
	"github.com/lightninglabs/darepo/rounds"
	"github.com/lightninglabs/darepo/timeout"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

const (
	// defaultConfTarget is the default confirmation target used for
	// fee estimation when not overridden by config.
	defaultConfTarget = 6

	// defaultMinConfs is the default minimum confirmation count
	// required for wallet UTXOs used in batch funding.
	defaultMinConfs = 1

	// defaultConfirmationTarget is the number of on-chain
	// confirmations required before transitioning a round to the
	// confirmed state.
	defaultConfirmationTarget = 1
)

// setupRoundsSubsystem initializes the timeout actor, batch watcher,
// and rounds actor. The rounds actor drives the round FSM lifecycle:
// registration, signing, broadcast, and confirmation. The batch
// watcher monitors confirmed batch transactions for tree-level events.
//
// This must be called after the indexer subsystem (step 5) so that the
// shared bridge and DB stores are available. The resulting actor
// references are stored on the Server for use by admin RPC handlers
// and dispatcher wiring.
func (s *Server) setupRoundsSubsystem(ctx context.Context) error {
	chainParams, err := networkToChainParams(s.cfg.Network)
	if err != nil {
		return fmt.Errorf("resolve chain params: %w", err)
	}

	// Register the shared timeout actor that provides wall-clock
	// timer scheduling for round phase deadlines.
	timeoutActor := timeout.NewActor()
	s.timeoutRef = actor.RegisterWithSystem(
		s.actorSystem, "timeout",
		actor.NewServiceKey[timeout.Msg, timeout.Resp](
			"timeout",
		),
		timeoutActor,
	)

	// Build DB-backed stores for rounds and VTXOs.
	dbStore := db.NewStore(
		s.db.DB(), s.db.Queries, s.db.Backend(),
		s.loggerFactory("RSTR"), nil,
	)
	roundStore := dbStore.NewRoundStore()
	vtxoStore := dbStore.NewVTXOStore()

	// Create the LND-backed wallet controller for PSBT funding and
	// signing.
	walletCtrl := lndbackend.NewLndWalletController(
		s.lnd.WalletKit, s.lnd.Signer,
	)

	// Use a static floor fee estimator. A future config phase can
	// wire the real LND fee estimator.
	feeEstimator := chainfee.NewStaticEstimator(
		chainfee.FeePerKwFloor, 0,
	)

	// Create the DB-backed VTXO locker for mutual exclusion across
	// rounds and OOR transfers.
	vtxoLocker := db.NewVTXOLockerDB(
		dbStore, s.loggerFactory("VTXOL"),
	)

	// Create and spawn the batch watcher actor for monitoring
	// confirmed batches on-chain.
	batchWatcherCfg := &batchwatcher.ActorConfig{
		Logger:      s.loggerFactory("BWTC"),
		ChainSource: s.chainSourceRef,
	}
	batchWatcher := batchwatcher.NewActor(batchWatcherCfg)
	s.batchWatcherRef = actor.RegisterWithSystem(
		s.actorSystem, "batch-watcher",
		actor.NewServiceKey[
			batchwatcher.BatchWatcherMsg,
			batchwatcher.BatchWatcherResp,
		]("batch-watcher"),
		batchWatcher,
	)

	// Set SelfRef after spawning (needed for callback mapping).
	batchWatcherCfg.SelfRef = s.batchWatcherRef

	// Build the rounds actor configuration. The terms are
	// placeholder defaults for now; a future config phase will
	// expose them via arkd flags and config file.
	//
	// TODO(roasbeef): Wire operator key, sweep key, forfeit
	// script, and full terms from server config once the key
	// management subsystem is in place.
	roundsCfg := &rounds.ActorConfig{
		ChainParams:        chainParams,
		Logger:             s.loggerFactory(rounds.Subsystem),
		Terms:              &batch.Terms{},
		ClientsConn:        s.clientBridge,
		ChainSourceActor:   s.chainSourceRef,
		TimeoutActor:       s.timeoutRef,
		RoundStore:         roundStore,
		VTXOStore:          vtxoStore,
		VTXOLocker:         vtxoLocker,
		WalletController:   walletCtrl,
		FeeEstimator:       feeEstimator,
		WalletAccount:      "",
		ConfTarget:         defaultConfTarget,
		MinConfs:           defaultMinConfs,
		ConfirmationTarget: defaultConfirmationTarget,
		BatchWatcher:       fn.Some(s.batchWatcherRef),
	}

	// Create and spawn the rounds actor.
	s.roundsActor = rounds.NewActor(roundsCfg)
	roundsKey := actor.NewServiceKey[
		rounds.ActorMsg, rounds.ActorResp,
	]("rounds-actor")
	s.roundsRef = roundsKey.Spawn(
		s.actorSystem, "rounds-actor", s.roundsActor,
	)

	// Set SelfRef on config after spawning (needed for timeout
	// callback mapping). ActorRef embeds TellOnlyRef, so we can
	// assign directly.
	roundsCfg.SelfRef = s.roundsRef

	// Start the rounds actor: loads pending rounds from storage
	// and creates a new live round accepting registrations.
	if err := s.roundsActor.Start(ctx); err != nil {
		return fmt.Errorf("start rounds actor: %w", err)
	}

	// Create the round operator that provides mailbox RPC
	// dispatchers for the per-client ingress loops. The local
	// mailbox edge client is shared with the indexer subsystem.
	edgeClient, err := newLocalMailboxClient(s.mailboxStore)
	if err != nil {
		return fmt.Errorf("build rounds edge client: %w", err)
	}

	s.roundsOperator, err = rounds.NewRoundOperator(
		rounds.RoundOperatorConfig{
			Edge:            edgeClient,
			SenderMailboxID: "svc:rounds",
			RoundsRef:       s.roundsRef,
		},
	)
	if err != nil {
		return fmt.Errorf("create rounds operator: %w", err)
	}

	log.InfoS(ctx, "Rounds subsystem initialized",
		slog.Uint64("conf_target",
			uint64(defaultConfTarget)))

	return nil
}

// stopRoundsSubsystem releases rounds-related resources. The actor
// system's Shutdown handles actor lifecycle; this method handles any
// additional cleanup.
func (s *Server) stopRoundsSubsystem(ctx context.Context) {
	if s.roundsActor != nil {
		log.InfoS(ctx, "Rounds subsystem stopped")
	}
}

// RoundsDispatchers returns the rounds operator's DispatcherMap for
// merging into per-client PerClientConfig.Dispatchers during client
// registration.
//
// Returns nil if the rounds subsystem has not been initialized.
func (s *Server) RoundsDispatchers() clientconn.DispatcherMap {
	if s.roundsOperator == nil {
		return nil
	}

	return s.roundsOperator.Dispatchers()
}

// networkToChainParams maps a network name to btcd chain parameters.
func networkToChainParams(network string) (*chaincfg.Params, error) {
	switch network {
	case "mainnet":
		return &chaincfg.MainNetParams, nil

	case "testnet":
		return &chaincfg.TestNet3Params, nil

	case "regtest":
		return &chaincfg.RegressionNetParams, nil

	case "simnet":
		return &chaincfg.SimNetParams, nil

	case "signet":
		return &chaincfg.SigNetParams, nil

	default:
		return nil, fmt.Errorf("unknown network %q", network)
	}
}
