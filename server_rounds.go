package darepo

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/google/uuid"
	"github.com/lightninglabs/darepo-client/arkrpc"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	mailboxpb "github.com/lightninglabs/darepo-client/mailbox/pb"
	"github.com/lightninglabs/darepo-client/rpc/roundpb"
	"github.com/lightninglabs/darepo-client/serverconn"
	"github.com/lightninglabs/darepo-client/timeout"
	"github.com/lightninglabs/darepo/batch"
	"github.com/lightninglabs/darepo/batchsweeper"
	"github.com/lightninglabs/darepo/batchwatcher"
	"github.com/lightninglabs/darepo/clientconn"
	"github.com/lightninglabs/darepo/db"
	"github.com/lightninglabs/darepo/indexer"
	"github.com/lightninglabs/darepo/ledger"
	"github.com/lightninglabs/darepo/lndbackend"
	"github.com/lightninglabs/darepo/oor"
	"github.com/lightninglabs/darepo/rounds"
	"github.com/lightningnetwork/lnd/clock"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"google.golang.org/protobuf/proto"
)

const (
	// keyFamilyArkSweep is a dedicated LND key family for the operator's
	// batch sweep key. Using a separate family from the MuSig2 operator key
	// avoids key-resolution ambiguity in the lndclient signing layer.
	keyFamilyArkSweep = keychain.KeyFamily(200)

	// keyFamilyArkOperator is the dedicated LND key family for the Ark
	// operator's MuSig2 tree-signing key. This must stay disjoint from all
	// client-side key families so a process using the same LND as both
	// server and client cannot derive the same x-only public key for both
	// signer roles. The non-zero index ensures lndclient sends both the
	// public key and locator to LND's signer.
	keyFamilyArkOperator = keychain.KeyFamily(201)
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
//
//nolint:funlen
func (s *Server) setupRoundsSubsystem(ctx context.Context) error {
	chainParams, err := networkToChainParams(s.cfg.Network)
	if err != nil {
		return fmt.Errorf("resolve chain params: %w", err)
	}

	// Register the shared timeout actor that provides wall-clock
	// timer scheduling for round phase deadlines. The behavior's
	// Start receives the registered ref so AfterFunc-driven fires
	// self-tell through the actor system mailbox rather than
	// mutating actor state from the clock goroutine.
	timeoutActor := timeout.NewActor()
	s.timeoutRef = actor.RegisterWithSystem(
		s.actorSystem, "timeout",
		actor.NewServiceKey[timeout.Msg, timeout.Resp](
			"timeout",
		),
		timeoutActor,
	)
	timeoutActor.Start(s.timeoutRef)

	// Build DB-backed stores for rounds and VTXOs using the
	// shared db.Store to avoid redundant wrappers.
	roundsLog := subLogger(s.cfg.Loggers, rounds.Subsystem)

	roundStore := s.db.NewRoundStore()
	vtxoStore := s.db.NewVTXOStore()

	// Create the LND-backed wallet controller for PSBT funding and
	// signing.
	walletCtrl := lndbackend.NewLndWalletController(
		s.lnd.WalletKit, s.lnd.Signer, fn.Some(roundsLog),
	)
	s.walletController = walletCtrl

	// Reuse the shared fee estimator wired by setupFeesSubsystem so
	// the rates the rounds actor uses to build round transactions
	// match the rates quoted to clients via EstimateFee.
	feeEstimator := s.feeEstimator

	// The batch watcher is spawned further below, after the operator
	// key has been derived and the OOR session store has been built.
	// This ordering is deliberate: wiring CheckpointLookup before the
	// actor starts removes the previous post-spawn mutation of
	// batchWatcherCfg.CheckpointLookup, which was both a Go data race
	// and a startup-window gap where a historical leaf spend replayed
	// via HeightHint could hit handleLeafSpend with
	// CheckpointLookup == None and silently drop the event.
	bwLog := subLogger(s.cfg.Loggers, batchwatcher.Subsystem)
	oorLog := subLogger(s.cfg.Loggers, oor.Subsystem)

	// Derive the operator key from Darepo's operator family. This is used
	// for MuSig2 tree signing and the connector address.
	operatorKeyDesc, err := s.lnd.WalletKit.DeriveKey(
		ctx, &keychain.KeyLocator{
			Family: keyFamilyArkOperator,
			Index:  1,
		},
	)
	if err != nil {
		return fmt.Errorf("derive operator key: %w", err)
	}

	// Derive the sweep key from a dedicated key family so it is
	// distinct from the operator signing key. Using a separate
	// family with a non-zero index ensures the lndclient signing
	// layer includes both the public key and key locator when
	// requesting signatures from LND.
	sweepKeyDesc, err := s.lnd.WalletKit.DeriveKey(
		ctx, &keychain.KeyLocator{
			Family: keyFamilyArkSweep,
			Index:  1,
		},
	)
	if err != nil {
		return fmt.Errorf("derive sweep key: %w", err)
	}

	// Build a taproot connector address from the operator key.
	outputKey := txscript.ComputeTaprootOutputKey(
		operatorKeyDesc.PubKey, nil,
	)
	connectorAddr, err := btcutil.NewAddressTaproot(
		schnorr.SerializePubKey(outputKey), chainParams,
	)
	if err != nil {
		return fmt.Errorf("create connector address: %w", err)
	}

	// Start with config-derived terms and overlay the LND-derived
	// key fields.
	rc := s.cfg.Rounds
	terms := roundsTermsFromConfig(rc)
	terms.OperatorKey = *operatorKeyDesc
	terms.SweepKey = *sweepKeyDesc
	terms.ConnectorAddress = connectorAddr

	// Derive the forfeit script from the operator key. This is a
	// P2TR script that clients reference in forfeit transactions.
	// The output key is the same one used for the connector
	// address above (key-spend-only, no script root).
	forfeitScript, err := txscript.PayToTaprootScript(outputKey)
	if err != nil {
		return fmt.Errorf("create forfeit script: %w", err)
	}

	// Store terms and forfeit script on the server so the
	// GetInfo RPC can return them to clients. Cache the
	// operator mailbox ID since the key is immutable.
	s.terms = terms
	s.forfeitScript = forfeitScript
	s.operatorMailboxID = serverconn.PubKeyMailboxID(
		terms.OperatorKey.PubKey,
	)
	if s.indexerService != nil {
		s.indexerService.SetVTXOProofPolicy(
			terms.OperatorKey.PubKey, terms.VTXOExitDelay,
		)
	}

	sessionStore, fraudRef, err := s.setupFraudPipeline(
		ctx, oorLog, roundStore, vtxoStore, terms,
	)
	if err != nil {
		return err
	}

	batchWatcherCfg := s.spawnBatchWatcher(
		bwLog, sessionStore, vtxoStore, fraudRef,
	)

	// Create the batch sweeper actor that reclaims expired
	// operator-controlled outputs back to the wallet. Wire it into
	// the already-spawned batch watcher through the adapter so the
	// watcher can stay agnostic of batchsweeper internals.
	bsLog := subLogger(s.cfg.Loggers, batchsweeper.Subsystem)
	batchSweeperCfg := &batchsweeper.ActorConfig{
		Log:          fn.Some(bsLog),
		BatchWatcher: s.batchWatcherRef,
		ChainSource:  s.chainSourceRef,
		SweepKey:     *sweepKeyDesc,
		SweepDelay:   terms.SweepDelay,
		Signer:       walletCtrl,
		LedgerRef: fn.Some[actor.TellOnlyRef[ledger.LedgerMsg]](
			s.ledgerRef,
		),
		NewSweepPkScript: func(ctx context.Context) ([]byte, error) {
			addr, err := s.lnd.WalletKit.NextAddr(
				ctx, "", walletrpc.AddressType_TAPROOT_PUBKEY,
				false,
			)
			if err != nil {
				return nil, err
			}

			return txscript.PayToAddrScript(addr)
		},
		TimeoutActor: fn.Some[actor.TellOnlyRef[timeout.Msg]](
			s.timeoutRef,
		),
		OnBatchSwept: func(ctx context.Context,
			vtxoOutpoints []wire.OutPoint) error {

			return vtxoStore.MarkVTXOsExpired(
				ctx, vtxoOutpoints,
			)
		},
	}
	batchSweeper := batchsweeper.NewActor(batchSweeperCfg)
	batchSweeperKey := actor.NewServiceKey[
		batchsweeper.Msg, batchsweeper.Resp,
	](
		"batch-sweeper-actor",
	)
	batchSweeperRef := batchSweeperKey.Spawn(
		s.actorSystem, "batch-sweeper-actor", batchSweeper,
	)

	// Set SelfRef before any expiry notifications can flow through the
	// watcher and then attach the notification adapter.
	batchSweeperCfg.SelfRef = batchSweeperRef
	batchWatcherCfg.BatchSweeper = fn.Some(
		batchsweeper.MapBatchWatcherNotification(batchSweeperRef),
	)

	if err := s.restoreConfirmedBatchWatches(ctx, roundStore); err != nil {
		return fmt.Errorf("restore confirmed batch watches: %w", err)
	}

	// Create a header verifier for TxProof validation using LND's
	// chain backend.
	headerVerifier := lndbackend.NewLndHeaderVerifier(
		s.lnd.ChainKit,
	)

	roundsCfg := &rounds.ActorConfig{
		ChainParams:         chainParams,
		Log:                 fn.Some(roundsLog),
		Terms:               terms,
		ForfeitScript:       forfeitScript,
		ClientsConn:         s.clientBridge,
		BoardingInputLocker: newInMemoryBoardingLocker(),
		ChainSource:         s.boardingChainSource,
		HeaderVerifier:      headerVerifier,
		ChainSourceActor:    s.chainSourceRef,
		TimeoutActor:        s.timeoutRef,
		RoundStore:          roundStore,
		VTXOStore:           vtxoStore,
		VTXOLocker:          s.vtxoLocker,
		WalletController:    walletCtrl,
		FeeEstimator:        feeEstimator,
		WalletAccount:       "",
		ConfTarget:          rc.ConfTarget,
		MinConfs:            rc.MinConfs,
		ConfirmationTarget:  rc.ConfirmationTarget,
		BatchWatcher:        fn.Some(s.batchWatcherRef),
		ShouldSeal:          sealPredicateFromConfig(rc),
		VTXOEventPublisher:  s.newVTXOEventPublisher(),
		FeeCalculator:       s.feeCalculator,
		TreasuryTracker:     s.treasury,
		LedgerRef:           s.ledgerRef,
		RoundTickInterval:   rc.RoundTickInterval,
	}

	// Create and spawn the rounds actor.
	s.roundsActor = rounds.NewActor(roundsCfg)
	roundsKey := actor.NewServiceKey[
		rounds.ActorMsg, rounds.ActorResp,
	](
		"rounds-actor",
	)
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

	// Register fire-and-forget dispatch routes for all round
	// RPCs on the shared event router. Each route deserializes
	// the envelope body, extracts the client ID from the
	// envelope sender, converts the proto to a domain actor
	// message, and Tell's the rounds actor.
	s.registerRoundRoutes(roundsKey)

	s.log.InfoS(ctx, "Rounds subsystem initialized",
		slog.Uint64("conf_target",
			uint64(rc.ConfTarget)))

	return nil
}

// stopRoundsSubsystem releases rounds-related resources. The actor
// system's Shutdown handles actor lifecycle; this method handles any
// additional cleanup.
func (s *Server) stopRoundsSubsystem(ctx context.Context) {
	if s.roundsActor != nil {
		s.log.InfoS(ctx, "Rounds subsystem stopped")
	}
}

// minConnectorTreeRadix mirrors the lower bound enforced by the
// connector tree builders in rounds/fsm_transitions.go (see
// buildConnectorDescriptors and buildConnectorTreesAndAssignments) and
// by tree.BuildConnectorTree. Any radix below this would crash the
// builder at finalization, so the startup gate refuses the config
// outright instead of admitting it and failing later.
const minConnectorTreeRadix = uint32(2)

// maxConnectorBroadcastTxs returns the worst-case number of connector
// transactions the operator's fraud-response broadcast must get
// confirmed before reaching the leaf tx that produces the connector
// dust spent by the stored forfeit transaction. This equals
// tree.Tree.Depth() for the connector tree shape the builder produces
// (one tx per tree level from the root spending the commitment-tx
// connector output down through the leaf).
//
// For a branching factor of radix and N leaves, the connector tree's
// depth is ceil(log_radix(N)) + 1 when N >= 2 (one extra level for the
// leaf), and 1 when N == 1 (the tree degenerates into a single leaf tx
// that consumes the commitment connector output directly). The +1 vs.
// pure ceil(log_radix(N)) is what the prior version missed: the leaf
// tx is itself a broadcast that must confirm before the CSV expires,
// not a free terminal symbol. See client/lib/tree/tree.go Tree.Depth
// and TestBuildTreeBFS for the canonical accounting.
func maxConnectorBroadcastTxs(maxConnectors, radix uint32) uint32 {
	if maxConnectors == 0 {
		return 0
	}

	// A single-leaf tree is one tx: the leaf itself spends the
	// commitment-tx connector output. Skip the log loop so callers
	// don't need to special-case the degenerate shape.
	if maxConnectors == 1 {
		return 1
	}

	if radix < minConnectorTreeRadix {
		return 0
	}

	// Count branch levels (ceil(log_radix(maxConnectors))) then add the
	// leaf level. capacity tracks the maximum number of leaves a tree
	// of `levels` branch tiers can hold.
	levels := uint32(0)
	capacity := uint32(1)
	for capacity < maxConnectors {
		capacity *= radix
		levels++
	}

	return levels + 1
}

// fraudResponseSafetyBlocks is the minimum number of blocks reserved
// between the worst-case connector path depth and the unilateral exit
// CSV maturity, so the txconfirm fee-bumper has room to escalate before
// a malicious client's exit becomes spendable. Sourced from ARK-04 §6.1.
const fraudResponseSafetyBlocks = uint32(6)

// checkFraudResponseSafetyMargin rejects round terms whose VTXOExitDelay
// is too short for the operator to walk the connector tree before a
// malicious client's unilateral exit (CSV = VTXOExitDelay) matures.
//
// The connector tree's depth is bounded by ConnectorTreeRadix — not
// TreeRadix, which only governs VTXO trees — because connector trees are
// built and persisted with the connector radix (see rounds/
// fsm_transitions.go). Using TreeRadix here would understate the
// connector path whenever TreeRadix > ConnectorTreeRadix, letting an
// operator boot with a VTXOExitDelay that loses the fraud-response race.
func checkFraudResponseSafetyMargin(terms *batch.Terms) error {
	// Reject ConnectorTreeRadix < 2 unconditionally because the
	// connector tree builders in rounds/fsm_transitions.go and
	// client/lib/tree/batch.go (BuildConnectorTree) fail at finalize
	// time for any radix below this — gating only on
	// MaxConnectorsPerTree > 1 would let a config that crashes the
	// first non-trivial forfeit round boot anyway.
	if terms.ConnectorTreeRadix < minConnectorTreeRadix {
		return fmt.Errorf("connector_tree_radix %d must be at least %d",
			terms.ConnectorTreeRadix, minConnectorTreeRadix)
	}

	maxTxs := maxConnectorBroadcastTxs(
		terms.MaxConnectorsPerTree, terms.ConnectorTreeRadix,
	)

	minExitDelay := maxTxs + fraudResponseSafetyBlocks
	if terms.VTXOExitDelay <= minExitDelay {
		return fmt.Errorf("vtxo_exit_delay %d insufficient for fraud "+
			"response: max connector broadcast txs %d + safety "+
			"margin %d requires vtxo_exit_delay > %d (blocks)",
			terms.VTXOExitDelay, maxTxs, fraudResponseSafetyBlocks,
			minExitDelay)
	}

	return nil
}

// setupFraudPipeline builds the OOR session store, derives the operator
// checkpoint policy, and spawns the fraud responder actor. Returns the
// session store (so the batchwatcher can be wired with it) and the
// FraudDetectorMsg ref the batchwatcher uses to push notifications
// (fn.None when the fraud responder is disabled). The structural
// validation (PackageSubmitter, VTXOExitDelay safety margin) runs
// regardless of Fraud.Disabled so a misconfigured deployment is caught at
// boot on every network.
//
// The third return is an fn.Option rather than a bare ref because the
// caller wraps it for the batchwatcher's FraudDetector config field, and
// fn.Some(nil-interface) silently constructs a non-None option that
// panics inside WhenSome on first delivery. Returning the option directly
// from the producer makes the disabled path impossible to mis-wire.
func (s *Server) setupFraudPipeline(ctx context.Context, oorLog btclog.Logger,
	roundStore *db.RoundStoreDB, vtxoStore *db.VTXOStoreDB,
	terms *batch.Terms) (*oor.DBSessionStore,
	fn.Option[actor.TellOnlyRef[batchwatcher.FraudDetectorMsg]], error) {

	// Hoist the None value so the wide generic instantiation lives in one
	// place and every return statement stays under the line-length limit.
	noFraud := fn.None[actor.TellOnlyRef[batchwatcher.FraudDetectorMsg]]()

	// Refuse to spawn the fraud responder without a v3/TRUC package
	// submitter. The fraud paths (checkpoint broadcast, timeout sweep)
	// use zero-fee parent + CPFP child packages that bitcoind only
	// admits via submitpackage; with no submitter the chain backend
	// returns "package submission not supported" on every attempt and
	// the operator silently fails to respond to fraud during a real
	// event. Surface this as a startup error so misconfiguration is
	// visible before the daemon serves clients.
	if s.cfg.PackageSubmitter == nil {
		return nil, noFraud, fmt.Errorf("fraud responder requires a " +
			"v3 package submitter; configure bitcoind RPC under " +
			"the bitcoind config section")
	}

	if err := checkFraudResponseSafetyMargin(terms); err != nil {
		return nil, noFraud, err
	}

	sessionStore := oor.NewDBSessionStore(
		s.db, clock.NewDefaultClock(), oorLog,
	)
	sessionStore.SetOperatorKey(terms.OperatorKey)

	// When fraud response is explicitly disabled (non-mainnet test
	// environments only — Validate refuses Disabled on mainnet), skip
	// spawning the fraud responder and return fn.None so the
	// batchwatcher does not notify any fraud detector. The structural
	// validation above still runs so a misconfigured vtxo_exit_delay is
	// caught at boot regardless. Log loud so operators see the disabled
	// state on every boot.
	if s.cfg.Fraud != nil && s.cfg.Fraud.Disabled {
		s.log.WarnS(ctx, "Operator fraud responder disabled — "+
			"on-chain fraud events will not be acted upon",
			nil)
		s.oorSessionStore = sessionStore

		return sessionStore, noFraud, nil
	}

	checkpointPolicy := arkscript.CheckpointPolicy{
		OperatorKey: terms.OperatorKey.PubKey,
		CSVDelay:    terms.VTXOExitDelay,
	}

	fraudRef, err := s.setupFraudResponder(
		roundStore, vtxoStore, sessionStore, terms.OperatorKey,
		checkpointPolicy,
	)
	if err != nil {
		return nil, noFraud, fmt.Errorf("setup fraud responder: %w",
			err)
	}

	// Publish the session store on the Server only once the fraud
	// pipeline has fully wired. Earlier returns above propagate
	// the construction error without leaving a half-initialized
	// store handle behind for later callers to find.
	s.oorSessionStore = sessionStore

	return sessionStore, fn.Some(fraudRef), nil
}

// spawnBatchWatcher constructs the batchwatcher actor with both recovery
// dependencies, spawns it, and sets SelfRef before the first message can
// arrive. Returns the live config pointer so the caller can wire BatchSweeper
// after the sweeper is spawned.
//
// fraudDetector is passed through as-is to the batchwatcher; the producer
// (setupFraudPipeline) returns fn.None when the fraud responder is
// disabled, so no nil-receiver interface ever reaches the batchwatcher's
// WhenSome callbacks.
func (s *Server) spawnBatchWatcher(bwLog btclog.Logger,
	sessionStore *oor.DBSessionStore, vtxoStore *db.VTXOStoreDB,
	fraudDetector fn.Option[actor.TellOnlyRef[batchwatcher.FraudDetectorMsg]]) *batchwatcher.ActorConfig {

	batchWatcherCfg := &batchwatcher.ActorConfig{
		Log:         fn.Some(bwLog),
		ChainSource: s.chainSourceRef,
		SpendRecoveryStore: fn.Some(
			newBatchWatcherSpendRecoveryStore(vtxoStore),
		),
		CheckpointLookup: fn.Some[batchwatcher.CheckpointLookup](
			newBatchWatcherCheckpointLookup(sessionStore),
		),
		CheckpointCSVDelay: s.terms.VTXOExitDelay,
		FraudDetector:      fraudDetector,
	}
	batchWatcher := batchwatcher.NewActor(batchWatcherCfg)

	bwKey := batchwatcher.NewServiceKey()
	s.batchWatcherRef = bwKey.Spawn(
		s.actorSystem, batchwatcher.BatchWatcherServiceKeyName,
		batchWatcher,
	)

	// Set SelfRef before the actor processes any messages, needed for
	// callback mapping in the batch watcher.
	batchWatcherCfg.SelfRef = s.batchWatcherRef

	return batchWatcherCfg
}

// restoreConfirmedBatchWatches re-registers confirmed round trees with the
// batch watcher after an operator restart.
func (s *Server) restoreConfirmedBatchWatches(ctx context.Context,
	roundStore *db.RoundStoreDB) error {

	confirmedRounds, err := roundStore.LoadConfirmedRounds(ctx)
	if err != nil {
		return err
	}

	var restored int
	for _, confirmed := range confirmedRounds {
		round := confirmed.Round
		if confirmed.ConfirmationHeight < 0 {
			return fmt.Errorf("round %s has negative confirmation "+
				"height %d", round.RoundID,
				confirmed.ConfirmationHeight)
		}

		confirmationHeight := uint32(confirmed.ConfirmationHeight)
		expiryHeight := confirmationHeight + round.CSVDelay

		// Reconstruct the descriptor used to derive the sweep
		// tapleaf at finalization. Post-migration rows yield a
		// fully populated descriptor. Pre-migration rows carry
		// only the pubkey; the sweeper compares it against its
		// configured key to decide whether the configured locator
		// is safe to use, and refuses to sign on mismatch rather
		// than silently using a rotated key.
		var sweepKey keychain.KeyDescriptor
		if round.SweepKey != nil {
			sweepKey = keychain.KeyDescriptor{
				PubKey: round.SweepKey,
			}
			if round.SweepKeyLocator != nil {
				sweepKey.KeyLocator = *round.SweepKeyLocator
			}
		}

		for outputIdx, vtxoTree := range round.VTXOTrees {
			batchID := batchwatcher.BatchIDForRoundOutput(
				uuid.UUID(round.RoundID), outputIdx,
			)

			err := s.batchWatcherRef.Tell(
				ctx, &batchwatcher.RegisterBatchRequest{
					BatchID:            batchID,
					Tree:               vtxoTree,
					ConfirmationHeight: confirmationHeight,
					ExpiryHeight:       expiryHeight,
					SweepKey:           sweepKey,
				},
			)
			if err != nil {
				return fmt.Errorf("register confirmed round "+
					"%s output %d: %w", round.RoundID,
					outputIdx, err)
			}

			restored++
		}
	}

	if restored > 0 {
		s.log.InfoS(ctx, "Restored confirmed batch watches",
			"batches", restored,
			"rounds", len(confirmedRounds),
		)
	}

	return nil
}

// roundsTermsFromConfig maps a RoundsConfig into a batch.Terms
// struct. Key-dependent fields (OperatorKey, SweepKey,
// ConnectorAddress) are left at their zero values.
func roundsTermsFromConfig(rc *RoundsConfig) *batch.Terms {
	return &batch.Terms{
		SweepDelay:           rc.SweepDelay,
		MaxVTXOsPerTree:      rc.MaxVTXOsPerTree,
		TreeRadix:            rc.TreeRadix,
		MaxConnectorsPerTree: rc.MaxConnectorsPerTree,
		ConnectorTreeRadix:   rc.ConnectorTreeRadix,
		ConnectorDustAmount: btcutil.Amount(
			rc.ConnectorDustAmount,
		),
		BoardingExitDelay:             rc.BoardingExitDelay,
		VTXOExitDelay:                 rc.VTXOExitDelay,
		RegistrationTimeout:           rc.RegistrationTimeout,
		FundPsbtLockDuration:          rc.FundPsbtLockDuration,
		BoardingExitDelaySafetyMargin: rc.BoardingExitDelaySafetyMargin,
		MinBoardingConfirmations:      rc.MinBoardingConfirmations,
		SignatureCollectionTimeout:    rc.SignatureCollectionTimeout,
		MinVTXOAmount:                 btcutil.Amount(rc.MinVTXOAmount),
		MaxVTXOAmount:                 btcutil.Amount(rc.MaxVTXOAmount),
		MinOperatorFee:                btcutil.Amount(rc.MinOperatorFee), //nolint:ll
	}
}

// registerRoundRoutes adds fire-and-forget dispatch routes for all
// five round RPCs to the server's shared EventRouter. Each route
// deserializes the envelope body into the expected proto type,
// extracts the client ID from the envelope sender, converts the
// proto to a domain actor message, and Tell's the rounds actor.
//
// This replaces the previous RoundOperator + ServeMux + Edge
// response pattern with the simpler AddEnvelopeRoute model. Since
// all round RPCs are fire-and-forget (the real response arrives
// asynchronously via the outbox event path), no response envelope
// needs to be built.
func (s *Server) registerRoundRoutes(
	roundsKey actor.ServiceKey[rounds.ActorMsg, rounds.ActorResp]) {

	RegisterRoundRoutes(s.eventRouter, roundsKey)
}

// RegisterRoundRoutes adds fire-and-forget dispatch routes for round
// RPCs (JoinRound, SubmitNonces, SubmitPartialSigs, SubmitForfeitSigs,
// SubmitVTXOForfeitSigs) to the given EventRouter. Each route
// deserializes the envelope body, converts the proto to a domain actor
// message, and Tell's the rounds actor.
//
// This is exported so the systest package can register the same routes
// on its own event router without duplicating route definitions.
func RegisterRoundRoutes( //nolint:funlen
	router *clientconn.EventRouter,
	roundsKey actor.ServiceKey[rounds.ActorMsg, rounds.ActorResp]) {
	svc := roundpb.ServiceName

	// JoinRound: client wants to join a round. This is the
	// only route that doesn't produce a RoundMsg wrapper,
	// since JoinRoundRequest is a top-level actor message.
	clientconn.AddEnvelopeRoute(
		router,
		clientconn.EnvelopeRouteConfig[
			rounds.ActorMsg, rounds.ActorResp,
		]{
			Service: svc,
			Method:  "JoinRound",
			NewEvent: func() proto.Message {
				return &roundpb.JoinRoundRequest{}
			},
			Key: roundsKey,
			Adapt: func(env *mailboxpb.Envelope, p proto.Message) (
				rounds.ActorMsg, error) {

				req, ok := p.(*roundpb.JoinRoundRequest) //nolint:ll
				if !ok {
					return nil, fmt.Errorf(
						"unexpected type %T", p)
				}

				domainReq, err :=
					rounds.JoinRoundRequestFromProto(
						req,
					)
				if err != nil {
					return nil, fmt.Errorf("parse join "+
						"request: %w", err)
				}

				return &rounds.JoinRoundRequest{
					ClientID: clientconn.ClientID(
						env.Sender,
					),
					Request: domainReq,
				}, nil
			},
		},
	)

	// SubmitNonces: client submits MuSig2 public nonces for a
	// round's VTXO tree transactions.
	clientconn.AddEnvelopeRoute(
		router,
		clientconn.EnvelopeRouteConfig[
			rounds.ActorMsg, rounds.ActorResp,
		]{
			Service: svc,
			Method:  "SubmitNonces",
			NewEvent: func() proto.Message {
				return &roundpb.SubmitNoncesRequest{}
			},
			Key: roundsKey,
			Adapt: func(env *mailboxpb.Envelope, p proto.Message) (
				rounds.ActorMsg, error) {

				req, ok := p.(*roundpb.SubmitNoncesRequest) //nolint:ll
				if !ok {
					return nil, fmt.Errorf(
						"unexpected type %T", p)
				}

				roundID, err := rounds.ParseRoundID(
					req.GetRoundId(),
				)
				if err != nil {
					return nil, fmt.Errorf("parse "+
						"round_id: %w", err)
				}

				nonces, err := rounds.NoncesFromProto(
					req.GetNonces(),
				)
				if err != nil {
					return nil, fmt.Errorf("parse "+
						"nonces: %w", err)
				}

				cID := clientconn.ClientID(env.Sender)

				return &rounds.RoundMsg{
					RoundID: roundID,
					Event: &rounds.ClientVTXONoncesEvent{ //nolint:ll
						ClientID: cID,
						Nonces:   nonces,
					},
				}, nil
			},
		},
	)

	// SubmitPartialSigs: client submits MuSig2 partial
	// signatures for a round's VTXO tree transactions.
	clientconn.AddEnvelopeRoute(
		router,
		clientconn.EnvelopeRouteConfig[
			rounds.ActorMsg, rounds.ActorResp,
		]{
			Service: svc,
			Method:  "SubmitPartialSigs",
			NewEvent: func() proto.Message {
				return &roundpb.SubmitPartialSigRequest{}
			},
			Key: roundsKey,
			Adapt: func(env *mailboxpb.Envelope, p proto.Message) (
				rounds.ActorMsg, error) {

				req, ok := p.(*roundpb.SubmitPartialSigRequest) //nolint:ll
				if !ok {
					return nil, fmt.Errorf(
						"unexpected type %T", p)
				}

				roundID, err := rounds.ParseRoundID(
					req.GetRoundId(),
				)
				if err != nil {
					return nil, fmt.Errorf("parse "+
						"round_id: %w", err)
				}

				sigs, err := rounds.PartialSigsFromProto(
					req.GetSignatures(),
				)
				if err != nil {
					return nil, fmt.Errorf("parse "+
						"signatures: %w", err)
				}

				cID := clientconn.ClientID(env.Sender)

				return &rounds.RoundMsg{
					RoundID: roundID,
					Event: &rounds.ClientVTXOPartialSigsEvent{ //nolint:ll
						ClientID:   cID,
						Signatures: sigs,
					},
				}, nil
			},
		},
	)

	// SubmitForfeitSigs: client submits boarding input
	// signatures (Schnorr) for on-chain inputs in a round.
	clientconn.AddEnvelopeRoute(
		router,
		clientconn.EnvelopeRouteConfig[
			rounds.ActorMsg, rounds.ActorResp,
		]{
			Service: svc,
			Method:  "SubmitForfeitSigs",
			NewEvent: func() proto.Message {
				return &roundpb.SubmitForfeitSigRequest{}
			},
			Key: roundsKey,
			Adapt: func(env *mailboxpb.Envelope, p proto.Message) (
				rounds.ActorMsg, error) {

				req, ok := p.(*roundpb.SubmitForfeitSigRequest) //nolint:ll
				if !ok {
					return nil, fmt.Errorf(
						"unexpected type %T", p)
				}

				roundID, err := rounds.ParseRoundID(
					req.GetRoundId(),
				)
				if err != nil {
					return nil, fmt.Errorf("parse "+
						"round_id: %w", err)
				}

				boardingSigs, err :=
					rounds.BoardingInputSigsFromProto(
						req.GetSignatures(),
					)
				if err != nil {
					return nil, fmt.Errorf("parse "+
						"boarding sigs: %w", err)
				}

				cID := clientconn.ClientID(env.Sender)

				return &rounds.RoundMsg{
					RoundID: roundID,
					Event: &rounds.ClientInputSignaturesEvent{ //nolint:ll
						ClientID:   cID,
						Signatures: boardingSigs,
					},
				}, nil
			},
		},
	)

	// SubmitVTXOForfeitSigs: client submits VTXO forfeit
	// transaction signatures for cooperative spend paths.
	clientconn.AddEnvelopeRoute(
		router,
		clientconn.EnvelopeRouteConfig[
			rounds.ActorMsg, rounds.ActorResp,
		]{
			Service: svc,
			Method:  "SubmitVTXOForfeitSigs",
			NewEvent: func() proto.Message {
				return &roundpb.SubmitVTXOForfeitSigsRequest{} //nolint:ll
			},
			Key: roundsKey,
			Adapt: func(env *mailboxpb.Envelope, p proto.Message) (
				rounds.ActorMsg, error) {

				req, ok := p.(*roundpb.SubmitVTXOForfeitSigsRequest) //nolint:ll
				if !ok {
					return nil, fmt.Errorf(
						"unexpected type %T", p)
				}

				roundID, err := rounds.ParseRoundID(
					req.GetRoundId(),
				)
				if err != nil {
					return nil, fmt.Errorf("parse "+
						"round_id: %w", err)
				}

				forfeitTxs, err :=
					rounds.ForfeitTxSigsFromProto(
						req.GetForfeitTxs(),
					)
				if err != nil {
					return nil, fmt.Errorf("parse forfeit "+
						"sigs: %w", err)
				}

				cID := clientconn.ClientID(env.Sender)

				return &rounds.RoundMsg{
					RoundID: roundID,
					Event: &rounds.ClientInputSignaturesEvent{ //nolint:ll
						ClientID:   cID,
						ForfeitTxs: forfeitTxs,
					},
				}, nil
			},
		},
	)

	// Quote-path envelope routes (accept / reject) for the #270
	// seal-time fee handshake. Grouped at the end of this
	// registrar so the quote wiring is legible as a unit.
	registerQuoteRoutes(router, roundsKey)
}

// registerQuoteRoutes registers envelope routes for the seal-time
// fee handshake accept / reject messages. Split out from
// RegisterRoundRoutes to keep the #270 envelope wiring together.
func registerQuoteRoutes(router *clientconn.EventRouter,
	roundsKey actor.ServiceKey[rounds.ActorMsg, rounds.ActorResp]) {

	svc := roundpb.ServiceName

	// AcceptQuote: client explicitly accepts a JoinRoundQuote. The
	// FSM in QuoteSentState validates the echoed quote_id and flips
	// the client's status to QuoteAccepted; advance to
	// BatchBuildingState happens once every pending client is
	// resolved.
	clientconn.AddEnvelopeRoute(
		router,
		clientconn.EnvelopeRouteConfig[
			rounds.ActorMsg, rounds.ActorResp,
		]{
			Service: svc,
			Method:  roundpb.MethodAcceptQuote,
			NewEvent: func() proto.Message {
				return &roundpb.JoinRoundAccept{}
			},
			Key: roundsKey,
			Adapt: func(env *mailboxpb.Envelope, p proto.Message) (
				rounds.ActorMsg, error) {

				req, ok := p.(*roundpb.JoinRoundAccept)
				if !ok {
					return nil, fmt.Errorf(
						"unexpected type %T", p)
				}

				roundID, err := parseRoundIDFromString(
					req.GetRoundId(),
				)
				if err != nil {
					return nil, fmt.Errorf("parse "+
						"round_id: %w", err)
				}

				cID := clientconn.ClientID(env.Sender)

				acceptEvt, err :=
					rounds.JoinRoundAcceptFromProto(
						cID, req,
					)
				if err != nil {
					return nil, fmt.Errorf("parse "+
						"accept: %w", err)
				}

				return &rounds.RoundMsg{
					RoundID: roundID,
					Event:   acceptEvt,
				}, nil
			},
		},
	)

	// RejectQuote: client explicitly rejects a JoinRoundQuote. The
	// FSM in QuoteSentState validates quote_id + flips the client's
	// status to QuoteRejected; the post-resolution path decides
	// reseal vs finalize-at-cap.
	clientconn.AddEnvelopeRoute(
		router,
		clientconn.EnvelopeRouteConfig[
			rounds.ActorMsg, rounds.ActorResp,
		]{
			Service: svc,
			Method:  roundpb.MethodRejectQuote,
			NewEvent: func() proto.Message {
				return &roundpb.JoinRoundReject{}
			},
			Key: roundsKey,
			Adapt: func(env *mailboxpb.Envelope, p proto.Message) (
				rounds.ActorMsg, error) {

				req, ok := p.(*roundpb.JoinRoundReject)
				if !ok {
					return nil, fmt.Errorf(
						"unexpected type %T", p)
				}

				roundID, err := parseRoundIDFromString(
					req.GetRoundId(),
				)
				if err != nil {
					return nil, fmt.Errorf("parse "+
						"round_id: %w", err)
				}

				cID := clientconn.ClientID(env.Sender)

				rejectEvt, err :=
					rounds.JoinRoundRejectFromProto(
						cID, req,
					)
				if err != nil {
					return nil, fmt.Errorf("parse "+
						"reject: %w", err)
				}

				return &rounds.RoundMsg{
					RoundID: roundID,
					Event:   rejectEvt,
				}, nil
			},
		},
	)
}

// parseRoundIDFromString parses a string-encoded round_id (UUID
// canonical form) into a rounds.RoundID. The accept/reject messages
// carry round_id as a string to line up with the wire-level quote
// message, so we translate at the envelope boundary.
func parseRoundIDFromString(s string) (rounds.RoundID, error) {
	u, err := uuid.Parse(s)
	if err != nil {
		return rounds.RoundID{}, fmt.Errorf("invalid round_id %q: %w",
			s, err)
	}

	return rounds.RoundID(u), nil
}

// sealPredicateFromConfig builds a composite seal predicate from the
// rounds configuration. Returns nil when no seal conditions are
// configured, which means the round only seals on registration timeout.
func sealPredicateFromConfig(rc *RoundsConfig) rounds.SealPredicate {
	var preds []rounds.SealPredicate

	if rc.MaxRoundClients > 0 {
		preds = append(
			preds,
			rounds.MaxClients(rc.MaxRoundClients),
		)
	}

	if rc.MaxRoundOutputAmount > 0 {
		preds = append(
			preds, rounds.MaxOutputAmount(rc.MaxRoundOutputAmount),
		)
	}

	switch len(preds) {
	case 0:
		return nil

	case 1:
		return preds[0]

	default:
		return rounds.AnySealPredicate(preds...)
	}
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

// vtxoEventPublisherAdapter implements rounds.VTXOEventPublisher by
// delegating to the indexer operator.
type vtxoEventPublisherAdapter struct {
	operator *indexer.Operator
}

func (a *vtxoEventPublisherAdapter) PublishVTXOCreated(ctx context.Context,
	pkScript []byte, outpoint wire.OutPoint, valueSat int64, roundID string,
	batchExpiry int32, relativeExpiry uint32, origin arkrpc.VTXOOrigin,
	commitmentTxid []byte) error {

	return a.operator.PublishVTXOEvent(
		ctx, pkScript,
		arkrpc.VTXOEventType_VTXO_EVENT_TYPE_CREATED,
		&arkrpc.OutPoint{
			Txid: outpoint.Hash[:],
			Vout: outpoint.Index,
		},
		arkrpc.VTXOStatus_VTXO_STATUS_LIVE,
		uint64(valueSat), roundID, batchExpiry,
		relativeExpiry, origin, commitmentTxid,
	)
}

// newVTXOEventPublisher builds the optional rounds→indexer event bridge.
// Returns nil if the indexer operator is not initialized.
func (s *Server) newVTXOEventPublisher() rounds.VTXOEventPublisher {
	if s.indexerOperator == nil {
		return nil
	}

	return &vtxoEventPublisherAdapter{
		operator: s.indexerOperator,
	}
}
