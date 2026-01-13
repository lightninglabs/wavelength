//nolint:ll
package round

import (
	"context"
	"encoding/hex"
	"fmt"
	"log/slog"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/baselib/protofsm"
	"github.com/lightninglabs/darepo-client/chainsource"
	"github.com/lightninglabs/darepo-client/lib/types"
	"github.com/lightninglabs/darepo-client/serverconn"
	"github.com/lightninglabs/darepo-client/wallet"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

// Compile-time assertion that RoundClientActor implements actor.Stoppable.
var _ actor.Stoppable = (*RoundClientActor)(nil)

// RoundFSM wraps a state machine instance for a specific round.
type RoundFSM struct {
	// FSM is the state machine for this round. The baselib protofsm uses 3
	// type parameters: InternalEvent, OutboxEvent, Env.
	FSM *ClientStateMachine

	// RoundID is the unique identifier for this round.
	RoundID RoundID

	// TxID is the commitment transaction ID for this round.
	TxID chainhash.Hash
}

// RoundClientActor wraps the client boarding FSM in an actor interface. The
// actor manages the FSM lifecycle, handles incoming actor messages, converts
// them to FSM events, processes outbox messages, and integrates with the
// chainsource actor for chain monitoring.
//
// Architecture:
//   - Actor holds FSM (protofsm.StateMachine).
//   - Actor receives actor messages (ClientMsg).
//   - Actor converts messages to FSM events.
//   - FSM processes events producing new state and outbox.
//   - Actor processes outbox by sending messages to server/chainsource.
type RoundClientActor struct {
	// cfg contains all the configuration for this actor.
	cfg *RoundClientConfig

	// log is the logger for this actor instance.
	log btclog.Logger

	// primaryFSM is the main FSM for assembling new rounds from Confirmed
	// intents. It handles the flow from Idle through round registration.
	primaryFSM *ClientStateMachine

	// activeRounds tracks concurrent round FSMs awaiting commitment tx
	// confirmation. Map: RoundID → RoundFSM instance.
	activeRounds map[RoundID]*RoundFSM

	// commitmentTxIndex maps commitment transaction IDs to their round IDs
	// for routing confirmation events. Map: CommitmentTxID → RoundID.
	commitmentTxIndex map[chainhash.Hash]RoundID

	// env is the FSM environment containing all dependencies.
	env *ClientEnvironment
}

// RoundClientConfig houses the configuration for a RoundClientActor.
type RoundClientConfig struct {
	// Name uniquely identifies this actor instance.
	Name string

	// Logger is the logger for this actor instance. If nil, uses the global
	// package logger.
	Logger btclog.Logger

	// Wallet provides MuSig2 signing capabilities needed for round
	// participation. Boarding address creation is handled by the wallet
	// actor.
	Wallet ClientWallet

	// RoundStore persists round coordination and checkpointing.
	RoundStore RoundStore

	// VTXOStore persists off-chain balance.
	VTXOStore VTXOStore

	// OperatorTerms contains the operator's parameters.
	OperatorTerms *types.OperatorTerms

	// ServerConn is a reference to the ServerConnectionActor for sending
	// messages to the Ark server.
	ServerConn actor.TellOnlyRef[serverconn.ServerConnMsg]

	// ChainSource is a reference to the ChainSource actor for registering
	// confirmation notifications for commitment transactions.
	// confirmation notifications for commitment transactions and querying
	// block height.
	ChainSource actor.ActorRef[chainsource.ChainSourceMsg, chainsource.ChainSourceResp]

	// WalletActor is a reference to the Ark wallet actor. The round actor
	// registers to receive BoardingUtxoConfirmedEvent notifications when
	// new boarding UTXOs are confirmed.
	WalletActor actor.ActorRef[wallet.WalletMsg, wallet.WalletResp]

	// SelfRef is a reference to this actor for receiving asynchronous
	// notifications (e.g., confirmations from ChainSource).
	SelfRef actor.TellOnlyRef[ClientMsg]

	// ChainParams are the Bitcoin network parameters.
	ChainParams *chaincfg.Params
}

// NewRoundClientActor creates a new client actor with the provided
// configuration. The actor starts in the Idle state.
//
// The FSM uses interfaces directly and calls lib package functions as needed.
// Chain operations are handled via outbox messages (not direct calls).
func NewRoundClientActor(cfg *RoundClientConfig) fn.Result[*RoundClientActor] {
	// Use the configured logger, falling back to the global package logger.
	actorLog := cfg.Logger
	if actorLog == nil {
		actorLog = log
	}

	// Create FSM environment with direct interface assignments. The FSM
	// will call lib functions directly when needed (e.g.,
	// lib.NewTreeSignerSession, signing helpers).
	env := &ClientEnvironment{
		RoundStore:    cfg.RoundStore,
		VTXOStore:     cfg.VTXOStore,
		Wallet:        cfg.Wallet,
		OperatorTerms: cfg.OperatorTerms,
		ChainParams:   cfg.ChainParams,
		Log:           actorLog,
	}

	if err := ValidateDelayParameters(
		cfg.OperatorTerms.SweepDelay, cfg.OperatorTerms.VTXOExitDelay,
	); err != nil {
		return fn.Err[*RoundClientActor](err)
	}

	// Create the FSM with Idle initial state. The baselib protofsm uses 3
	// type parameters: InternalEvent, OutboxEvent, Env.
	fsmCfg := ClientStateMachineCfg{
		Logger:        actorLog.WithPrefix("fsm-primary"),
		ErrorReporter: newContextErrorReporter(context.Background(), "fsm-primary"),
		InitialState:  &Idle{},
		Env:           env,
	}
	primaryFSM := protofsm.NewStateMachine(fsmCfg)
	primaryFSM.Start(context.Background())

	return fn.Ok(&RoundClientActor{
		cfg:               cfg,
		log:               actorLog,
		primaryFSM:        &primaryFSM,
		activeRounds:      make(map[RoundID]*RoundFSM),
		commitmentTxIndex: make(map[chainhash.Hash]RoundID),
		env:               env,
	})
}

// createRoundFSM creates a new FSM instance for a specific round, restoring
// from checkpointed state. Uses FetchState to load both round data and FSM
// state atomically.
func (a *RoundClientActor) createRoundFSM(ctx context.Context,
	roundID RoundID) (*RoundFSM, error) {

	round, state, err := a.cfg.RoundStore.FetchState(ctx, roundID)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch round state: %w", err)
	}

	fsmPrefix := fmt.Sprintf("fsm-%s", round.RoundID)
	fsmLogger := a.log.WithPrefix(fsmPrefix)

	env := &ClientEnvironment{
		RoundStore:    a.cfg.RoundStore,
		VTXOStore:     a.cfg.VTXOStore,
		Wallet:        a.cfg.Wallet,
		OperatorTerms: a.cfg.OperatorTerms,
		ChainParams:   a.cfg.ChainParams,
		Log:           fsmLogger,
	}
	fsmCfg := ClientStateMachineCfg{
		Logger:        fsmLogger,
		ErrorReporter: newContextErrorReporter(ctx, fsmPrefix),
		InitialState:  state,
		Env:           env,
	}
	fsm := protofsm.NewStateMachine(fsmCfg)
	fsm.Start(ctx)

	a.log.InfoS(ctx, "Created round FSM from checkpoint",
		slog.String("round_id", round.RoundID.String()),
		slog.String("initial_state", state.String()))

	txid := fn.MapOptionZ(
		round.CommitmentTx, func(p *psbt.Packet) chainhash.Hash {
			return p.UnsignedTx.TxHash()
		},
	)

	return &RoundFSM{
		FSM:     &fsm,
		RoundID: round.RoundID,
		TxID:    txid,
	}, nil
}

// registerCommitmentConfirmation registers for confirmation monitoring of a
// commitment transaction with the chainsource actor.
func (a *RoundClientActor) registerCommitmentConfirmation(ctx context.Context,
	txid chainhash.Hash) {

	callerID := fmt.Sprintf("commitment-tx-%s", txid.String())

	mappedRef := chainsource.MapConfirmationEvent(
		a.cfg.SelfRef,
		func(ce chainsource.ConfirmationEvent) ClientMsg {
			return &ConfirmationEvent{
				Txid:          ce.Txid,
				BlockHeight:   ce.BlockHeight,
				Confirmations: ce.NumConfs,
				Tx:            ce.Tx,
			}
		},
	)

	confReq := &chainsource.RegisterConfRequest{
		CallerID:    callerID,
		Txid:        &txid,
		TargetConfs: a.cfg.OperatorTerms.MinConfirmations,
		NotifyActor: fn.Some(mappedRef),
	}

	a.cfg.ChainSource.Tell(ctx, confReq)
}

// askEventAndProcessOutbox sends an event to the FSM and processes any
// emitted outbox messages. This consolidates a common pattern throughout
// the actor where FSM events trigger outbox processing.
func (a *RoundClientActor) askEventAndProcessOutbox(
	ctx context.Context, fsm *ClientStateMachine, event ClientEvent) error {

	future := fsm.AskEvent(ctx, event)
	result := future.Await(ctx)

	events, err := result.Unpack()
	if err != nil {
		return err
	}

	if len(events) > 0 {
		if err := a.processOutbox(ctx, events); err != nil {
			return fmt.Errorf("failed to process outbox: %w", err)
		}
	}

	return nil
}

// OnStop implements actor.Stoppable to gracefully shut down all FSMs when the
// actor is stopping. This prevents goroutine leaks by stopping the primaryFSM
// and all active round FSMs.
func (a *RoundClientActor) OnStop(ctx context.Context) error {
	a.log.InfoS(ctx, "Stopping round client actor",
		slog.Int("active_rounds", len(a.activeRounds)))

	// Stop the primary FSM.
	a.primaryFSM.Stop()

	// Stop all active round FSMs.
	for roundID, roundFSM := range a.activeRounds {
		a.log.DebugS(ctx, "Stopping round FSM",
			slog.String("round_id", roundID.String()))

		roundFSM.FSM.Stop()
	}

	a.log.InfoS(ctx, "Round client actor stopped")

	return nil
}

// Start initializes the actor by registering with the wallet actor to receive
// boarding UTXO confirmation notifications, and resuming any active rounds.
// This should be called once after actor creation to restore state.
func (a *RoundClientActor) Start(ctx context.Context) error {
	a.log.InfoS(ctx, "Starting round client actor",
		slog.String("name", a.cfg.Name))

	// Register with the wallet actor to receive BoardingUtxoConfirmedEvent
	// notifications. The wallet handles all boarding address monitoring and
	// will notify us when new UTXOs are confirmed.
	mappedRef := actor.NewMapInputRef(
		a.cfg.SelfRef,
		func(evt wallet.BoardingUtxoConfirmedEvent) ClientMsg {
			return &WalletBoardingConfirmed{
				Intent: evt.BoardingIntent,
			}
		},
	)

	// Request all historical confirmations. The wallet will send backlog
	// events for any confirmed intents.
	regReq := &wallet.RegisterConfirmationNotifierRequest{
		NotifierID:    fmt.Sprintf("round-actor-%s", a.cfg.Name),
		NotifyActor:   mappedRef,
		BacklogHeight: fn.None[int32](),
		MinConf:       fn.Some(a.cfg.OperatorTerms.MinConfirmations),
	}

	future := a.cfg.WalletActor.Ask(ctx, regReq)
	result := future.Await(ctx)
	if result.IsErr() {
		return fmt.Errorf("register with wallet: %w", result.Err())
	}

	a.log.InfoS(ctx, "Registered with wallet actor for boarding confirmations",
		slog.Int("min_confirmations", int(a.cfg.OperatorTerms.MinConfirmations)))

	// Load active rounds (commitment tx broadcast, not yet confirmed) and
	// resume their FSMs.
	activeRounds, err := a.cfg.RoundStore.ListActiveRounds(ctx)
	if err != nil {
		return fmt.Errorf("failed to load active rounds: %w", err)
	}

	a.log.InfoS(ctx, "Loaded active rounds from database",
		slog.Int("count", len(activeRounds)))

	for _, round := range activeRounds {
		roundFSM, err := a.createRoundFSM(ctx, round.RoundID)
		if err != nil {
			return fmt.Errorf("failed to create FSM for "+
				"round %s: %w", round.RoundID, err)
		}

		a.activeRounds[round.RoundID] = roundFSM

		// Register for confirmation of the commitment tx for this
		// round.
		if !roundFSM.TxID.IsEqual(&chainhash.Hash{}) {
			a.commitmentTxIndex[roundFSM.TxID] = round.RoundID
			a.registerCommitmentConfirmation(ctx, roundFSM.TxID)

			a.log.InfoS(ctx, "Resumed round awaiting confirmation",
				slog.String("round_id", round.RoundID.String()),
				slog.String("commitment_txid", roundFSM.TxID.String()))
		}
	}

	a.log.InfoS(ctx, "Round client actor started")

	return nil
}

// Receive processes an actor message and returns a response. This is the main
// entry point for the actor.
func (a *RoundClientActor) Receive(ctx context.Context,
	msg ClientMsg) fn.Result[ClientResp] {

	switch m := msg.(type) {
	case *WalletBoardingConfirmed:
		return a.handleWalletBoardingConfirmed(ctx, m)

	case *ServerMessageNotification:
		return a.handleServerMessage(ctx, m)

	case *GetClientStateRequest:
		return a.handleGetState(ctx, m)

	case *CancelRoundRequest:
		return a.handleCancelRound(ctx, m)

	case *ConfirmationEvent:
		return a.handleConfirmation(ctx, m)

	default:
		return fn.Err[ClientResp](fmt.Errorf(
			"unknown message type: %T", msg))
	}
}

// handleWalletBoardingConfirmed processes a boarding UTXO confirmation event
// from the wallet actor. This creates the FSM event and drives the state
// machine forward. The wallet handles all persistence; we just react.
func (a *RoundClientActor) handleWalletBoardingConfirmed(ctx context.Context,
	msg *WalletBoardingConfirmed) fn.Result[ClientResp] {

	walletIntent := msg.Intent
	if walletIntent == nil {
		return fn.Err[ClientResp](fmt.Errorf(
			"wallet boarding confirmed event missing intent"))
	}

	a.log.InfoS(ctx, "Received boarding UTXO confirmation from wallet",
		btclog.Fmt("outpoint", "%v", walletIntent.Outpoint),
		slog.Int("amount", int(walletIntent.ChainInfo.Amount)),
		slog.Int("conf_height", int(walletIntent.ChainInfo.ConfHeight)))

	// Create the FSM event from the wallet's confirmed intent. Wallet only
	// notifies after min confs, so we set confirmations to 1. Include the
	// Address and TxProof for building the BoardingRequest.
	confirmEvt := &BoardingUTXOConfirmed{
		Outpoint:      walletIntent.Outpoint,
		Address:       walletIntent.Address,
		BlockHeight:   walletIntent.ChainInfo.ConfHeight,
		BlockHash:     walletIntent.ChainInfo.ConfHash,
		Confirmations: int32(a.cfg.OperatorTerms.MinConfirmations),
		Tx:            walletIntent.ChainInfo.ConfTx,
		TxProof:       walletIntent.ChainInfo.TxProof,
	}

	// Drive the FSM with the confirmation event.
	err := a.askEventAndProcessOutbox(ctx, a.primaryFSM, confirmEvt)
	if err != nil {
		return fn.Err[ClientResp](fmt.Errorf(
			"FSM error processing boarding confirmation: %w", err))
	}

	return fn.Ok[ClientResp](nil)
}

// migrateRoundToActiveFSM creates a dedicated FSM for a checkpointed round.
// This is called when the primary FSM emits a RoundCheckpointedNotification,
// signaling that a round has been saved to storage and should be migrated to
// its own FSM instance for independent tracking.
func (a *RoundClientActor) migrateRoundToActiveFSM(ctx context.Context,
	roundID RoundID) error {

	// Check if this round is already in activeRounds (idempotency).
	if _, exists := a.activeRounds[roundID]; exists {
		return nil
	}

	// Create FSM for this round (loads round + state via FetchState).
	roundFSM, err := a.createRoundFSM(ctx, roundID)
	if err != nil {
		return fmt.Errorf("failed to create round FSM: %w", err)
	}
	a.activeRounds[roundID] = roundFSM

	a.log.InfoS(ctx, "Migrated round to dedicated FSM",
		slog.String("round_id", roundID.String()))

	// Index commitment tx and register for confirmation monitoring.
	if !roundFSM.TxID.IsEqual(&chainhash.Hash{}) {
		a.commitmentTxIndex[roundFSM.TxID] = roundID
		a.registerCommitmentConfirmation(ctx, roundFSM.TxID)
	}

	return nil
}

// extractRoundID returns the RoundID from events that carry one. Returns the
// zero value for events without a RoundID field.
func extractRoundID(event ClientEvent) (RoundID, bool) {
	switch e := event.(type) {
	case *RoundJoined:
		return e.RoundID, true

	case *CommitmentTxBuilt:
		return e.RoundID, true

	case *NoncesAggregated:
		return e.RoundID, true

	case *OperatorSigned:
		return e.RoundID, true

	case *AwaitingBoardingSigs:
		return e.RoundID, true

	default:
		return RoundID{}, false
	}
}

// handleServerMessage processes a message from the server (delivered via
// Outbox). The actor routes the message to the appropriate FSM based on the
// RoundID: if the round exists in activeRounds, route there; otherwise use
// the primaryFSM.
func (a *RoundClientActor) handleServerMessage(ctx context.Context,
	msg *ServerMessageNotification) fn.Result[ClientResp] {

	// Determine which FSM should handle this message. If the event has a
	// RoundID and that round exists in activeRounds, route to that FSM.
	// Otherwise, use the primaryFSM.
	targetFSM := a.primaryFSM
	targetName := "primary"

	roundID, hasRoundID := extractRoundID(msg.Message)
	if hasRoundID {
		if roundFSM, exists := a.activeRounds[roundID]; exists {
			targetFSM = roundFSM.FSM
			targetName = roundID.String()
		}
	}

	a.log.DebugS(ctx, "Received server message",
		slog.String("event_type", fmt.Sprintf("%T", msg.Message)),
		slog.String("target_fsm", targetName))

	err := a.askEventAndProcessOutbox(ctx, targetFSM, msg.Message)
	if err != nil {
		return fn.Err[ClientResp](fmt.Errorf(
			"FSM error processing server message: %w", err))
	}

	return fn.Ok[ClientResp](&ServerMessageResponse{
		Success: true,
	})
}

// handleGetState returns the current FSM state for monitoring/debugging.
// This includes both the primary FSM and all active round FSMs.
func (a *RoundClientActor) handleGetState(ctx context.Context,
	_ *GetClientStateRequest) fn.Result[ClientResp] {

	states := make(map[string]FSMStateInfo)

	// Query the primary FSM state.
	primaryState, err := a.primaryFSM.CurrentState()
	if err != nil {
		return fn.Err[ClientResp](fmt.Errorf(
			"failed to get primary FSM state: %w", err))
	}

	// Add primary FSM to the response map.
	clientState, ok := primaryState.(ClientState)
	if !ok {
		return fn.Err[ClientResp](fmt.Errorf(
			"primary FSM state is not a ClientState"))
	}

	states["primary"] = FSMStateInfo{
		State:     clientState,
		IsPrimary: true,
		RoundID:   RoundID{},
	}

	for roundID, roundFSM := range a.activeRounds {
		roundState, err := roundFSM.FSM.CurrentState()
		if err != nil {
			a.log.WarnS(ctx, "Failed to get FSM state for round", err,
				slog.String("round_id", roundID.String()),
			)

			continue
		}

		clientState, ok := roundState.(ClientState)
		if !ok {
			a.log.WarnS(ctx, "Round FSM state is not a ClientState", nil,
				slog.String("round_id", roundID.String()),
				slog.String("state_type", fmt.Sprintf("%T", roundState)),
			)

			continue
		}

		states[roundID.String()] = FSMStateInfo{
			State:     clientState,
			IsPrimary: false,
			RoundID:   roundID,
		}
	}

	return fn.Ok[ClientResp](&GetClientStateResponse{
		States: states,
	})
}

// handleCancelRound attempts to cancel the current round participation.
func (a *RoundClientActor) handleCancelRound(ctx context.Context,
	req *CancelRoundRequest) fn.Result[ClientResp] {

	a.log.InfoS(ctx, "Cancelling round participation by user request")

	// Inject a BoardingFailed event to transition the FSM to failed state.
	// This will trigger any cleanup logic in the FSM transitions.
	cancelEvent := &BoardingFailed{
		Reason:      "User requested cancellation",
		Error:       fmt.Errorf("round cancelled by user"),
		Recoverable: true,
	}

	err := a.askEventAndProcessOutbox(ctx, a.primaryFSM, cancelEvent)
	if err != nil {
		a.log.WarnS(ctx, "Failed to cancel round", err)

		return fn.Ok[ClientResp](&CancelRoundResponse{
			Success: false,
			Error:   fmt.Sprintf("failed to cancel: %v", err),
		})
	}

	a.log.InfoS(ctx, "Round participation cancelled successfully")

	return fn.Ok[ClientResp](&CancelRoundResponse{
		Success: true,
	})
}

// onRoundComplete is called when a round finishes successfully. This removes
// the round from active tracking and archives the round data.
func (a *RoundClientActor) onRoundComplete(ctx context.Context, roundID RoundID,
	txid chainhash.Hash, confInfo ConfInfo) error {

	a.log.InfoS(ctx, "Round completed successfully",
		slog.String("round_id", roundID.String()),
		slog.String("commitment_txid", txid.String()),
		slog.Int("conf_height", int(confInfo.Height)))

	delete(a.activeRounds, roundID)
	delete(a.commitmentTxIndex, txid)

	return a.cfg.RoundStore.FinalizeRound(ctx, roundID, txid, confInfo)
}

// handleConfirmation processes a commitment transaction confirmation event
// from ChainSource. Boarding address confirmations are now handled via
// WalletBoardingConfirmed events from the wallet actor.
//
// Concurrency: The actor framework serializes all messages through Receive(),
// so no synchronization is needed for activeRounds map access.
func (a *RoundClientActor) handleConfirmation(ctx context.Context,
	event *ConfirmationEvent) fn.Result[ClientResp] {

	a.log.InfoS(ctx, "Received commitment transaction confirmation",
		slog.String("txid", event.Txid.String()),
		slog.Int("block_height", int(event.BlockHeight)),
		slog.Int("confirmations", int(event.Confirmations)))

	// Look up the round by commitment transaction ID.
	round, err := a.cfg.RoundStore.LookupRoundByCommitmentTx(ctx, event.Txid)
	if err != nil {
		// Not a commitment tx we're tracking. This shouldn't happen
		// since we only register for commitment tx confirmations.
		// Log for observability in case of database issues.
		a.log.WarnS(ctx, "LookupRoundByCommitmentTx failed", err,
			slog.String("txid", event.Txid.String()))

		return fn.Ok[ClientResp](nil)
	}

	// Route to the specific round's FSM.
	roundFSM, exists := a.activeRounds[round.RoundID]
	if !exists {
		return fn.Err[ClientResp](fmt.Errorf(
			"round FSM not found for round %s", round.RoundID))
	}

	a.log.InfoS(ctx, "Routing confirmation to round FSM",
		slog.String("round_id", round.RoundID.String()))

	confirmEvt := &BoardingConfirmed{
		TxID:          event.Txid,
		BlockHeight:   event.BlockHeight,
		BlockHash:     event.BlockHash,
		Confirmations: int32(event.Confirmations),
	}

	err = a.askEventAndProcessOutbox(ctx, roundFSM.FSM, confirmEvt)
	if err != nil {
		return fn.Err[ClientResp](fmt.Errorf(
			"FSM error processing commitment confirmation: %w", err))
	}

	return fn.Ok[ClientResp](nil)
}

// processOutbox processes messages emitted by the FSM via Outbox and routes
// them to the appropriate destination (server or chainsource).
func (a *RoundClientActor) processOutbox(ctx context.Context,
	outbox []ClientOutMsg) error {

	for _, msg := range outbox {
		// Check if this message should be sent to the server. All
		// server-bound messages implement the ServerMessage interface.
		if serverMsg, ok := msg.(serverconn.ServerMessage); ok {
			sendReq := &serverconn.SendClientEventRequest{
				Message: serverMsg,
			}
			a.cfg.ServerConn.Tell(ctx, sendReq)

			continue
		}

		// Handle non-server messages.
		switch m := msg.(type) {
		case *RegisterConfirmationRequest:
			// FSM emitted a confirmation request. Complete it with
			// the NotifyActor field pointing to ourselves and send
			// to ChainSource.
			var sessionID string
			switch {
			case len(m.PkScript) > 0:
				sessionID = hex.EncodeToString(m.PkScript)

			case m.Txid != nil:
				sessionID = m.Txid.String()

			default:
				sessionID = "unknown"
			}
			callerID := fmt.Sprintf(
				"boarding-%s-%s", sessionID, m.CallerID,
			)

			// Use the shared mapper helper so ChainSource can
			// deliver confirmation events directly without an
			// intermediate actor.
			mappedRef := chainsource.MapConfirmationEvent(
				a.cfg.SelfRef,
				func(ce chainsource.ConfirmationEvent) ClientMsg {
					return &ConfirmationEvent{
						Txid:          ce.Txid,
						BlockHeight:   ce.BlockHeight,
						Confirmations: ce.NumConfs,
						Tx:            ce.Tx,
					}
				},
			)

			// Build the complete RegisterConfRequest with the
			// mapper as the NotifyActor target.
			confReq := &chainsource.RegisterConfRequest{
				CallerID:    callerID,
				Txid:        m.Txid,
				PkScript:    m.PkScript,
				TargetConfs: m.TargetConfs,
				HeightHint:  m.HeightHint,
				NotifyActor: fn.Some(mappedRef),
			}

			a.cfg.ChainSource.Tell(ctx, confReq)

		case *VTXOCreatedNotification:
			_ = m.VTXOs

		case *RoundCompletedNotification:
			a.log.InfoS(ctx, "Processing round completion notification",
				slog.String("round_id", m.RoundID.String()),
				slog.String("txid", m.TxID.String()))

			// Round FSM reached ConfirmedState. Perform actor
			// cleanup.
			err := a.onRoundComplete(
				ctx, m.RoundID, m.TxID, m.ConfInfo,
			)
			if err != nil {
				return fmt.Errorf("failed to complete "+
					"round %s: %w", m.RoundID, err)
			}

		case *RoundCheckpointedNotification:
			a.log.InfoS(ctx, "Processing round checkpoint notification",
				slog.String("round_id", m.RoundID.String()))

			// Primary FSM checkpointed a round. Migrate to
			// dedicated FSM.
			err := a.migrateRoundToActiveFSM(ctx, m.RoundID)
			if err != nil {
				return fmt.Errorf("failed to migrate "+
					"round %s: %w", m.RoundID, err)
			}

		case *RoundFailedNotification:
			// Round entered failed state. Log for observability.
			roundIDStr := "none"
			m.RoundID.WhenSome(func(id RoundID) {
				roundIDStr = id.String()
			})
			a.log.WarnS(ctx, "Round failed", nil,
				slog.String("round_id", roundIDStr),
				slog.String("reason", m.Reason),
				slog.Bool("recoverable", m.Recoverable))

		default:
			// Unknown outbox message type. Log for debugging.
			a.log.DebugS(ctx, "Ignoring unknown outbox message type",
				slog.String("type", fmt.Sprintf("%T", msg)),
			)
		}
	}

	return nil
}
