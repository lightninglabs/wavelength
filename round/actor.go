//nolint:ll
package round

import (
	"context"
	"encoding/hex"
	"fmt"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
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
	RoundID string

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

	// primaryFSM is the main FSM for assembling new rounds from Confirmed
	// intents. It handles the flow from Idle through round registration.
	primaryFSM *ClientStateMachine

	// activeRounds tracks concurrent round FSMs awaiting commitment tx
	// confirmation. Map: RoundID → RoundFSM instance.
	activeRounds map[string]*RoundFSM

	// commitmentTxIndex maps commitment transaction IDs to their round IDs
	// for routing confirmation events. Map: CommitmentTxID → RoundID.
	commitmentTxIndex map[chainhash.Hash]string

	// env is the FSM environment containing all dependencies.
	env *ClientEnvironment
}

// RoundClientConfig houses the configuration for a RoundClientActor.
type RoundClientConfig struct {
	// Name uniquely identifies this actor instance.
	Name string

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
	ChainSource actor.TellOnlyRef[chainsource.ChainSourceMsg]

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
	// Create FSM environment with direct interface assignments. The FSM
	// will call lib functions directly when needed (e.g.,
	// lib.NewTreeSignerSession, signing helpers).
	env := &ClientEnvironment{
		RoundStore:    cfg.RoundStore,
		VTXOStore:     cfg.VTXOStore,
		Wallet:        cfg.Wallet,
		OperatorTerms: cfg.OperatorTerms,
		ChainParams:   cfg.ChainParams,
	}

	if err := ValidateDelayParameters(
		cfg.OperatorTerms.SweepDelay, cfg.OperatorTerms.VTXOExitDelay,
	); err != nil {
		return fn.Err[*RoundClientActor](err)
	}

	// Create the FSM with Idle initial state. The baselib protofsm uses 3
	// type parameters: InternalEvent, OutboxEvent, Env.
	fsmCfg := ClientStateMachineCfg{
		Logger:        log.WithPrefix("fsm-primary"),
		ErrorReporter: newContextErrorReporter(context.Background(), "fsm-primary"),
		InitialState:  &Idle{},
		Env:           env,
	}
	primaryFSM := protofsm.NewStateMachine(fsmCfg)
	primaryFSM.Start(context.Background())

	return fn.Ok(&RoundClientActor{
		cfg:               cfg,
		primaryFSM:        &primaryFSM,
		activeRounds:      make(map[string]*RoundFSM),
		commitmentTxIndex: make(map[chainhash.Hash]string),
		env:               env,
	})
}

// createRoundFSM creates a new FSM instance for a specific round, restoring
// from checkpointed state. Uses FetchState to load both round data and FSM
// state atomically.
func (a *RoundClientActor) createRoundFSM(ctx context.Context,
	roundID string) (*RoundFSM, error) {

	round, state, err := a.cfg.RoundStore.FetchState(ctx, roundID)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch round state: %w", err)
	}
	env := &ClientEnvironment{
		RoundStore:    a.cfg.RoundStore,
		VTXOStore:     a.cfg.VTXOStore,
		Wallet:        a.cfg.Wallet,
		OperatorTerms: a.cfg.OperatorTerms,
		ChainParams:   a.cfg.ChainParams,
	}

	fsmPrefix := fmt.Sprintf("fsm-%s", round.RoundID)
	fsmCfg := ClientStateMachineCfg{
		Logger:        log.WithPrefix(fsmPrefix),
		ErrorReporter: newContextErrorReporter(ctx, fsmPrefix),
		InitialState:  state,
		Env:           env,
	}
	fsm := protofsm.NewStateMachine(fsmCfg)
	fsm.Start(ctx)

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
func (a *RoundClientActor) OnStop(_ context.Context) error {
	// Stop the primary FSM.
	a.primaryFSM.Stop()

	// Stop all active round FSMs.
	for _, roundFSM := range a.activeRounds {
		roundFSM.FSM.Stop()
	}

	return nil
}

// Start initializes the actor by registering with the wallet actor to receive
// boarding UTXO confirmation notifications, and resuming any active rounds.
// This should be called once after actor creation to restore state.
func (a *RoundClientActor) Start(ctx context.Context) error {
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

	// Load active rounds (commitment tx broadcast, not yet confirmed) and
	// resume their FSMs.
	activeRounds, err := a.cfg.RoundStore.ListActiveRounds(ctx)
	if err != nil {
		return fmt.Errorf("failed to load active rounds: %w", err)
	}

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
		}
	}

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
	roundID string) error {

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

	// Index commitment tx and register for confirmation monitoring.
	if !roundFSM.TxID.IsEqual(&chainhash.Hash{}) {
		a.commitmentTxIndex[roundFSM.TxID] = roundID
		a.registerCommitmentConfirmation(ctx, roundFSM.TxID)
	}

	return nil
}

// extractRoundID returns the RoundID from events that carry one. Returns empty
// string for events without a RoundID field.
func extractRoundID(event ClientEvent) string {
	switch e := event.(type) {
	case *RoundJoined:
		return e.RoundID

	case *CommitmentTxBuilt:
		return e.RoundID

	case *NoncesAggregated:
		return e.RoundID

	case *OperatorSigned:
		return e.RoundID

	default:
		return ""
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

	roundID := extractRoundID(msg.Message)
	if roundID != "" {
		if roundFSM, exists := a.activeRounds[roundID]; exists {
			targetFSM = roundFSM.FSM
		}
	}

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
func (a *RoundClientActor) handleGetState(_ context.Context,
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
		RoundID:   "",
	}

	for roundID, roundFSM := range a.activeRounds {
		roundState, err := roundFSM.FSM.CurrentState()
		if err != nil {
			log.Warnf("Failed to get FSM state for round %s: %v",
				roundID, err)

			continue
		}

		clientState, ok := roundState.(ClientState)
		if !ok {
			log.Warnf("Round %s FSM state is not a ClientState: %T",
				roundID, roundState)

			continue
		}

		states[roundID] = FSMStateInfo{
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

	// Inject a BoardingFailed event to transition the FSM to failed state.
	// This will trigger any cleanup logic in the FSM transitions.
	cancelEvent := &BoardingFailed{
		Reason:      "User requested cancellation",
		Error:       fmt.Errorf("round cancelled by user"),
		Recoverable: true,
	}

	err := a.askEventAndProcessOutbox(ctx, a.primaryFSM, cancelEvent)
	if err != nil {
		return fn.Ok[ClientResp](&CancelRoundResponse{
			Success: false,
			Error:   fmt.Sprintf("failed to cancel: %v", err),
		})
	}

	return fn.Ok[ClientResp](&CancelRoundResponse{
		Success: true,
	})
}

// onRoundComplete is called when a round finishes successfully. This removes
// the round from active tracking and archives the round data.
func (a *RoundClientActor) onRoundComplete(ctx context.Context, roundID string,
	txid chainhash.Hash) error {

	delete(a.activeRounds, roundID)
	delete(a.commitmentTxIndex, txid)

	return a.cfg.RoundStore.FinalizeRound(ctx, roundID, txid)
}

// handleConfirmation processes a commitment transaction confirmation event
// from ChainSource. Boarding address confirmations are now handled via
// WalletBoardingConfirmed events from the wallet actor.
//
// Concurrency: The actor framework serializes all messages through Receive(),
// so no synchronization is needed for activeRounds map access.
func (a *RoundClientActor) handleConfirmation(ctx context.Context,
	event *ConfirmationEvent) fn.Result[ClientResp] {

	// Look up the round by commitment transaction ID.
	round, err := a.cfg.RoundStore.LookupRoundByCommitmentTx(ctx, event.Txid)
	if err != nil {
		// Not a commitment tx we're tracking. This shouldn't happen
		// since we only register for commitment tx confirmations.
		// Log for observability in case of database issues.
		log.Warnf("LookupRoundByCommitmentTx failed for txid %s: %v",
			event.Txid, err)

		return fn.Ok[ClientResp](nil)
	}

	// Route to the specific round's FSM.
	roundFSM, exists := a.activeRounds[round.RoundID]
	if !exists {
		return fn.Err[ClientResp](fmt.Errorf(
			"round FSM not found for round %s", round.RoundID))
	}

	confirmEvt := &BoardingConfirmed{
		TxID:          event.Txid,
		BlockHeight:   event.BlockHeight,
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
			// Round FSM reached ConfirmedState. Perform actor
			// cleanup.
			err := a.onRoundComplete(ctx, m.RoundID, m.TxID)
			if err != nil {
				return fmt.Errorf("failed to complete "+
					"round %s: %w", m.RoundID, err)
			}

		case *RoundCheckpointedNotification:
			// Primary FSM checkpointed a round. Migrate to
			// dedicated FSM.
			err := a.migrateRoundToActiveFSM(ctx, m.RoundID)
			if err != nil {
				return fmt.Errorf("failed to migrate "+
					"round %s: %w", m.RoundID, err)
			}

		case *RoundFailedNotification:
			// Round entered failed state. Log for observability.
			log.Warnf("Round %s failed: %s (recoverable=%v)",
				m.RoundID, m.Reason, m.Recoverable)

		default:
			// Unknown outbox message type. Log for debugging.
			log.Debugf("Ignoring unknown outbox message type: %T",
				msg)
		}
	}

	return nil
}
