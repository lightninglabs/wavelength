package rounds

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btclog/v2"
	"github.com/google/uuid"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/baselib/protofsm"
	"github.com/lightninglabs/darepo-client/chainsource"
	"github.com/lightninglabs/darepo/batch"
	"github.com/lightninglabs/darepo/clientconn"
	"github.com/lightninglabs/darepo/timeout"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

// ActorConfig contains the configuration parameters for the rounds actor.
type ActorConfig struct {
	// ChainParams are the Bitcoin network parameters.
	ChainParams *chaincfg.Params

	// Logger is used for logging.
	Logger btclog.Logger

	// Terms are the operator terms for the round.
	Terms *batch.Terms

	// ForfeitScript is the output script that clients must use for the
	// penalty output in forfeit transactions. This allows the server to
	// claim forfeited VTXO funds.
	ForfeitScript []byte

	// ClientsConn is a reference to the ClientsConnectionActor for sending
	// messages to registered clients.
	ClientsConn actor.TellOnlyRef[clientconn.ClientConnMsg]

	// BoardingInputLocker provides global locking of boarding inputs
	// across concurrent rounds.
	BoardingInputLocker BoardingInputLocker

	// ChainSource provides access to on-chain data. If not set, the FSM
	// will not be able to validate UTXOs.
	ChainSource ChainSource

	// TimeoutActor is a reference to the timeout scheduling actor.
	TimeoutActor actor.TellOnlyRef[timeout.Msg]

	// SelfRef is a reference to this actor for receiving asynchronous
	// notifications (e.g., timeout expirations). The actor uses this to
	// create mapped references for callback registration.
	SelfRef actor.TellOnlyRef[ActorMsg]

	// WalletController provides access to wallet operations for batch
	// building.
	WalletController WalletController

	// FeeEstimator provides fee rate estimation for transactions.
	FeeEstimator chainfee.Estimator

	// WalletAccount is the wallet account to use for funding batch
	// transactions.
	WalletAccount string

	// ConfTarget is the confirmation target to use for fee estimation.
	ConfTarget uint32

	// MinConfs is the minimum number of confirmations required for wallet
	// UTXOs to be used for funding.
	MinConfs int32

	// RoundStore provides persistent storage for rounds.
	RoundStore RoundStore

	// VTXOStore provides persistent storage for VTXOs.
	VTXOStore VTXOStore

	// ChainSourceActor is a reference to the chain source actor for
	// broadcasting transactions and subscribing to confirmations.
	ChainSourceActor actor.ActorRef[
		chainsource.ChainSourceMsg, chainsource.ChainSourceResp,
	]

	// ConfirmationTarget is the number of confirmations required before
	// transitioning a round to the confirmed state.
	ConfirmationTarget uint32
}

// Actor is the server rounds actor. It wraps the round FSM and manages its
// lifecycle. It tracks multiple concurrent rounds - one "current" round that
// accepts new registrations, and sealed rounds that are still being processed.
type Actor struct {
	// cfg contains all the configuration for this actor.
	cfg *ActorConfig

	// currentRound is the current live round FSM instance managed by the
	// actor. This is the round that is actively accepting new registrations.
	currentRound *RoundFSM

	// rounds is a map of all rounds being tracked by the actor, keyed by
	// round ID. This includes the current round and any sealed rounds that
	// are still being processed.
	rounds map[RoundID]*RoundFSM

	log btclog.Logger
}

// makeTimeoutID creates a composite timeout ID from a round ID and phase.
// The format is "roundID:phase" which allows the actor to identify both which
// round the timeout belongs to and which phase scheduled it.
func makeTimeoutID(roundID RoundID, phase TimeoutPhase) timeout.ID {
	return timeout.ID(fmt.Sprintf("%s:%s", uuid.UUID(roundID), phase))
}

// parseTimeoutID extracts the round ID and phase from a composite timeout ID.
// Returns an error if the ID format is invalid.
func parseTimeoutID(id timeout.ID) (RoundID, TimeoutPhase, error) {
	parts := strings.SplitN(string(id), ":", 2)
	if len(parts) != 2 {
		return RoundID{}, "", fmt.Errorf("invalid timeout ID "+
			"format: %s", id)
	}

	roundUUID, err := uuid.Parse(parts[0])
	if err != nil {
		return RoundID{}, "", fmt.Errorf("invalid round ID in "+
			"timeout ID: %w", err)
	}

	return RoundID(roundUUID), TimeoutPhase(parts[1]), nil
}

// NewActor creates a new server rounds actor with the provided configuration.
// It will check the rounds-store for any rounds that still need to be tracked
// and resume them. It will create a new "live" round that will accept new
// registrations.
func NewActor(cfg *ActorConfig) fn.Result[*Actor] {
	return fn.Ok(&Actor{
		cfg:    cfg,
		log:    cfg.Logger,
		rounds: make(map[RoundID]*RoundFSM),
	})
}

// Start initializes the actor. It loads any pending rounds from storage that
// need to be tracked until confirmation, then creates a new live round FSM to
// accept registrations.
func (a *Actor) Start(ctx context.Context) error {
	// Load previous rounds from storage that still need to be managed
	// (e.g., rounds awaiting confirmation).
	if err := a.loadPendingRounds(ctx); err != nil {
		return fmt.Errorf("unable to load pending rounds: %w", err)
	}

	// Create a new round to accept registrations.
	round, err := a.newRoundFSM(ctx)
	if err != nil {
		return fmt.Errorf("unable to create new round FSM: %w", err)
	}

	a.currentRound = round
	a.rounds[round.RoundID] = round

	return nil
}

// loadPendingRounds loads all pending rounds from storage and creates FSMs for
// them. These are rounds that have been finalized but not yet confirmed
// on-chain.
func (a *Actor) loadPendingRounds(ctx context.Context) error {
	rounds, err := a.cfg.RoundStore.LoadPendingRounds(ctx)
	if err != nil {
		return fmt.Errorf("failed to load pending rounds: %w", err)
	}

	for _, round := range rounds {
		roundFSM, err := a.loadRoundFSM(ctx, round)
		if err != nil {
			return fmt.Errorf("failed to load round %s: %w",
				round.RoundID, err)
		}

		a.rounds[round.RoundID] = roundFSM

		a.log.InfoS(ctx, "Loaded pending round from storage",
			"round_id", round.RoundID)
	}

	return nil
}

// loadRoundFSM creates a new FSM for a persisted round, starting in
// FinalizedState. This is used to restore rounds that were finalized but not
// yet confirmed on-chain. After creating the FSM, it re-subscribes to
// confirmation notifications for the round's transaction.
func (a *Actor) loadRoundFSM(ctx context.Context, round *Round) (*RoundFSM,
	error) {

	// Create the FSM starting in FinalizedState since the round was already
	// signed and persisted.
	initialState := &FinalizedState{
		ClientRegistrations: round.ClientRegistrations,
		FinalTx:             round.FinalTx,
		VTXOTrees:           round.VTXOTrees,
		ForfeitInfos:        round.ForfeitInfos,
	}

	fsm := a.buildAndStartRoundFSM(ctx, round.RoundID, initialState)

	// Re-subscribe to confirmation notifications.
	broadcastReq := &BroadcastRoundReq{
		RoundID:  round.RoundID,
		SignedTx: round.FinalTx,
	}
	if err := a.broadcastAndSubscribe(ctx, broadcastReq); err != nil {
		return nil, fmt.Errorf("failed to re-subscribe to "+
			"confirmation: %w", err)
	}

	return fsm, nil
}

func (a *Actor) buildAndStartRoundFSM(ctx context.Context, roundID RoundID,
	state State) *RoundFSM {

	fsmPrefix := roundID.LogPrefix()
	fsmLogger := a.cfg.Logger.WithPrefix(fsmPrefix)

	env := &Environment{
		RoundID:             roundID,
		Log:                 fsmLogger,
		ChainParams:         a.cfg.ChainParams,
		BoardingInputLocker: a.cfg.BoardingInputLocker,
		ChainSource:         a.cfg.ChainSource,
		Terms:               a.cfg.Terms,
		ForfeitScript:       a.cfg.ForfeitScript,
		WalletController:    a.cfg.WalletController,
		FeeEstimator:        a.cfg.FeeEstimator,
		WalletAccount:       a.cfg.WalletAccount,
		ConfTarget:          a.cfg.ConfTarget,
		MinConfs:            a.cfg.MinConfs,
		RoundStore:          a.cfg.RoundStore,
		VTXOStore:           a.cfg.VTXOStore,
	}

	fsmCfg := StateMachineCfg{
		InitialState:  state,
		Env:           env,
		Logger:        a.log.WithPrefix(fsmPrefix),
		ErrorReporter: newLoggingErrorReporter(fsmLogger),
	}
	fsm := protofsm.NewStateMachine(fsmCfg)
	fsm.Start(ctx)

	return &RoundFSM{
		FSM:     &fsm,
		RoundID: roundID,
	}
}

// Receive processes an actor message and returns a response. This is the main
// entry point for the actor.
func (a *Actor) Receive(ctx context.Context,
	msg ActorMsg) fn.Result[ActorResp] {

	a.log.DebugS(ctx, "Received actor message",
		slog.String("msg_type", msg.MessageType()))

	switch m := msg.(type) {
	case *JoinRoundRequest:
		return a.handleJoinRoundRequest(ctx, m)

	case *TimeoutMsg:
		return a.handleTimeout(ctx, m)

	case *RoundMsg:
		return a.handleRoundEvent(ctx, m)

	case *ConfirmationMsg:
		return a.handleConfirmation(ctx, m)

	default:
		a.log.WarnS(ctx, "Unknown message type", nil,
			slog.String("msg_type", msg.MessageType()))

		return fn.Err[ActorResp](fmt.Errorf(
			"unknown message type: %T", m))
	}
}

// handleRoundEvent processes RoundMsg messages by forwarding the contained
// Event to the specified round's FSM.
func (a *Actor) handleRoundEvent(ctx context.Context,
	msg *RoundMsg) fn.Result[ActorResp] {

	round := a.getRound(msg.RoundID)
	if round == nil {
		return fn.Err[ActorResp](fmt.Errorf("round %s not found",
			msg.RoundID))
	}

	err := a.askEventAndProcessOutbox(ctx, round.FSM, msg.Event)
	if err != nil {
		return fn.Err[ActorResp](fmt.Errorf(
			"FSM error processing event: %w", err))
	}

	return fn.Ok[ActorResp](nil)
}

// getRound returns the round FSM for the given round ID, or nil if not found.
func (a *Actor) getRound(roundID RoundID) *RoundFSM {
	return a.rounds[roundID]
}

// askEventAndProcessOutbox sends an event to the FSM and processes any emitted
// outbox messages. This consolidates a common pattern throughout the actor
// where FSM events trigger outbox processing.
func (a *Actor) askEventAndProcessOutbox(ctx context.Context, fsm *StateMachine,
	event Event) error {

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

// processOutbox processes messages emitted by the FSM via Outbox and routes
// them to the appropriate destination.
func (a *Actor) processOutbox(ctx context.Context, outbox []OutboxEvent) error {
	for _, msg := range outbox {
		switch m := msg.(type) {
		// Check if this message should be sent to client(s). All
		// client-bound messages implement the ClientMessage interface.
		case clientconn.ClientMessage:
			sendReq := &clientconn.SendServerEventRequest{
				Message: m,
			}
			a.cfg.ClientsConn.Tell(ctx, sendReq)

		case *StartTimeoutReq:
			// Create composite timeout ID that includes the phase.
			// This allows us to identify which state scheduled the
			// timeout when it expires.
			compositeID := makeTimeoutID(m.RoundID, m.Phase)

			// MapTimeoutExpired creates a callback that converts
			// timeout.ExpiredMsg to our TimeoutMsg. The phase is
			// encoded in the composite ID and will be parsed by
			// handleTimeout.
			callbackRef := timeout.MapTimeoutExpired(
				a.cfg.SelfRef,
				func(expired timeout.ExpiredMsg) ActorMsg {
					return &TimeoutMsg{
						TimeoutID: expired.ID,
					}
				},
			)

			// Send schedule request to timeout actor with
			// composite ID.
			req := &timeout.ScheduleTimeoutRequest{
				ID:       compositeID,
				Duration: m.Duration,
				Callback: callbackRef,
			}
			a.cfg.TimeoutActor.Tell(ctx, req)

		case *CancelTimeoutReq:
			// Cancel timeout using composite ID constructed from
			// round ID and phase.
			compositeID := makeTimeoutID(m.RoundID, m.Phase)
			cancelReq := &timeout.CancelTimeoutRequest{
				ID: compositeID,
			}
			a.cfg.TimeoutActor.Tell(ctx, cancelReq)

		case *RoundSealedReq:
			// Round has been sealed - create a new round for new
			// registrations.
			newRound, err := a.newRoundFSM(ctx)
			if err != nil {
				return fmt.Errorf(
					"failed to create new round: %w", err)
			}

			a.currentRound = newRound
			a.rounds[newRound.RoundID] = newRound

			a.log.InfoS(ctx, "Created new round after sealing",
				"sealed_round", m.SealedRoundID,
				"new_round", newRound.RoundID)

		case *RoundFailedReq:
			// Round has failed - clean up and create a new round if
			// this was the current round.
			a.log.ErrorS(ctx, "Round failed",
				fmt.Errorf("round failed: %s", m.Reason),
				"round_id", m.FailedRoundID,
				"reason", m.Reason)

			// Remove the failed round from tracking.
			delete(a.rounds, m.FailedRoundID)

			// If this was the current round, create a new one.
			if a.currentRound != nil &&
				a.currentRound.RoundID == m.FailedRoundID {

				newRound, err := a.newRoundFSM(ctx)
				if err != nil {
					return fmt.Errorf("failed to create "+
						"new round after failure: %w",
						err)
				}

				a.currentRound = newRound
				a.rounds[newRound.RoundID] = newRound

				a.log.InfoS(ctx, "Created new round after "+
					"failure",
					"failed_round", m.FailedRoundID,
					"new_round", newRound.RoundID)
			}

		case *BroadcastRoundReq:
			// Broadcast the transaction and subscribe to
			// confirmation.
			//
			// TODO(elle): Handle broadcast failures - if broadcast
			// fails, we should retry, fee bump or transition to a
			// failed state.
			err := a.broadcastAndSubscribe(ctx, m)
			if err != nil {
				return fmt.Errorf("failed to broadcast "+
					"round %s: %w", m.RoundID, err)
			}

		default:
			// Unknown outbox message. This could be an internal FSM
			// event that doesn't need routing, so we ignore it.
			_ = m
		}
	}

	return nil
}

// newRoundFSM creates and starts a new round FSM instance with a unique round
// ID and returns it.
func (a *Actor) newRoundFSM(ctx context.Context) (*RoundFSM, error) {
	roundID, err := NewRoundID()
	if err != nil {
		return nil, fmt.Errorf("unable to generate round ID: %w", err)
	}

	return a.buildAndStartRoundFSM(ctx, roundID, &CreatedState{}), nil
}

// handleJoinRoundRequest processes a JoinRoundRequest message by forwarding it
// to the current round FSM.
func (a *Actor) handleJoinRoundRequest(ctx context.Context,
	msg *JoinRoundRequest) fn.Result[ActorResp] {

	// Convert the actor message to an FSM event.
	joinEvent := &ClientJoinRequestEvent{
		ClientID: msg.ClientID,
		Request:  msg.Request,
	}

	err := a.askEventAndProcessOutbox(ctx, a.currentRound.FSM, joinEvent)
	if err != nil {
		return fn.Err[ActorResp](fmt.Errorf(
			"FSM error processing join request: %w", err))
	}

	return fn.Ok[ActorResp](nil)
}

// handleTimeout processes a timeout message by parsing the composite timeout
// ID to extract the round ID and phase, then sending the appropriate
// phase-specific timeout event to the round's FSM.
func (a *Actor) handleTimeout(ctx context.Context,
	msg *TimeoutMsg) fn.Result[ActorResp] {

	// Parse the composite timeout ID to get round ID and phase.
	roundID, phase, err := parseTimeoutID(msg.TimeoutID)
	if err != nil {
		a.log.WarnS(ctx, "Failed to parse timeout ID", err,
			"timeout_id", string(msg.TimeoutID))

		return fn.Ok[ActorResp](nil)
	}

	// Find the round for this timeout.
	round := a.getRound(roundID)
	if round == nil {
		// Stale timeout for unknown round, ignore.
		a.log.DebugS(ctx, "Ignoring timeout for unknown round",
			"round_id", roundID,
			"phase", phase)

		return fn.Ok[ActorResp](nil)
	}

	// Create the appropriate phase-specific timeout event.
	var timeoutEvent Event
	switch phase {
	case TimeoutPhaseRegistration:
		timeoutEvent = &RegistrationTimeoutEvent{}

	case TimeoutPhaseInputSigs:
		timeoutEvent = &InputSignaturesTimeoutEvent{}

	case TimeoutPhaseVTXONonces:
		timeoutEvent = &VTXONoncesTimeoutEvent{}

	case TimeoutPhaseVTXOSignatures:
		timeoutEvent = &VTXOSignaturesTimeoutEvent{}

	default:
		// Unknown phase - log warning and ignore.
		a.log.WarnS(ctx, "Ignoring timeout with unknown phase", nil,
			"round_id", roundID,
			"phase", phase)

		return fn.Ok[ActorResp](nil)
	}

	// Send the phase-specific timeout event to the FSM.
	err = a.askEventAndProcessOutbox(ctx, round.FSM, timeoutEvent)
	if err != nil {
		return fn.Err[ActorResp](fmt.Errorf(
			"FSM error processing %s timeout: %w", phase, err))
	}

	return fn.Ok[ActorResp](nil)
}

// handleConfirmation processes a ConfirmationMsg by forwarding a
// TransactionConfirmedEvent to the appropriate round's FSM.
func (a *Actor) handleConfirmation(ctx context.Context,
	msg *ConfirmationMsg) fn.Result[ActorResp] {

	// Find the round for this confirmation.
	round := a.getRound(msg.RoundID)
	if round == nil {
		// Round no longer tracked - this can happen if the round was
		// cleaned up before the confirmation arrived.
		a.log.WarnS(ctx, "Ignoring confirmation for unknown round", nil,
			"round_id", msg.RoundID)

		return fn.Ok[ActorResp](nil)
	}

	// Forward the confirmation event to the FSM.
	confirmedEvent := &TransactionConfirmedEvent{
		BlockHeight: msg.BlockHeight,
		BlockHash:   msg.BlockHash,
		NumConfs:    msg.NumConfs,
	}

	err := a.askEventAndProcessOutbox(ctx, round.FSM, confirmedEvent)
	if err != nil {
		return fn.Err[ActorResp](fmt.Errorf(
			"FSM error processing confirmation: %w", err))
	}

	a.log.InfoS(ctx, "Round transaction confirmed",
		"round_id", msg.RoundID,
		"block_height", msg.BlockHeight,
		"num_confs", msg.NumConfs)

	return fn.Ok[ActorResp](nil)
}

// broadcastAndSubscribe broadcasts the round's signed transaction and
// subscribes to confirmation notifications.
func (a *Actor) broadcastAndSubscribe(ctx context.Context,
	req *BroadcastRoundReq) error {

	// Skip if ChainSourceActor is not configured (e.g., in tests).
	if a.cfg.ChainSourceActor == nil {
		a.log.DebugS(ctx, "Skipping broadcast - no chain source actor",
			"round_id", req.RoundID)

		return nil
	}

	// Step 1: Broadcast the transaction.
	txHash := req.SignedTx.TxHash()
	broadcastReq := &chainsource.BroadcastTxRequest{
		Tx:    req.SignedTx,
		Label: fmt.Sprintf("round-%s", req.RoundID),
	}

	broadcastFuture := a.cfg.ChainSourceActor.Ask(ctx, broadcastReq)
	broadcastResult := broadcastFuture.Await(ctx)
	if _, err := broadcastResult.Unpack(); err != nil {
		return fmt.Errorf("broadcast failed: %w", err)
	}

	a.log.InfoS(ctx, "Broadcast round transaction",
		"round_id", req.RoundID,
		"txid", txHash.String())

	// Get the current block height to use as a height hint for the
	// confirmation subscription. LND requires a height hint > 0 to optimize
	// confirmation scanning.
	heightFuture := a.cfg.ChainSourceActor.Ask(ctx, &chainsource.BestHeightRequest{})
	heightResult := heightFuture.Await(ctx)
	heightResp, err := heightResult.Unpack()
	if err != nil {
		return fmt.Errorf("get best height: %w", err)
	}
	bestHeightResp := heightResp.(*chainsource.BestHeightResponse)

	// Subscribe to confirmation using actor mode. We create a mapped
	// reference that transforms a ConfirmationEvent to a ConfirmationMsg.
	// We use Tell (fire-and-forget) since we handle the confirmation
	// asynchronously via ConfirmationMsg.
	callbackRef := chainsource.MapConfirmationEvent(
		a.cfg.SelfRef,
		func(event chainsource.ConfirmationEvent) ActorMsg {
			return &ConfirmationMsg{
				RoundID:     req.RoundID,
				BlockHeight: event.BlockHeight,
				BlockHash:   event.BlockHash,
				NumConfs:    event.NumConfs,
			}
		},
	)

	// Use the round ID as the caller ID for deterministic cancellation.
	// LND requires a pkScript for confirmation tracking - use the first
	// output (the batch output) since that's what we're watching.
	var pkScript []byte
	if len(req.SignedTx.TxOut) > 0 {
		pkScript = req.SignedTx.TxOut[0].PkScript
	}

	confReq := &chainsource.RegisterConfRequest{
		CallerID:    req.RoundID.String(),
		Txid:        &txHash,
		PkScript:    pkScript,
		TargetConfs: a.cfg.ConfirmationTarget,
		HeightHint:  uint32(bestHeightResp.Height),
		NotifyActor: fn.Some(callbackRef),
	}

	a.cfg.ChainSourceActor.Tell(ctx, confReq)

	a.log.DebugS(ctx, "Subscribed to transaction confirmation",
		"round_id", req.RoundID,
		"txid", txHash.String(),
		"target_confs", a.cfg.ConfirmationTarget)

	return nil
}
