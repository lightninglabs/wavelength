package rounds

import (
	"context"
	"fmt"
	"strings"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btclog/v2"
	"github.com/google/uuid"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/baselib/protofsm"
	"github.com/lightninglabs/darepo/batch"
	"github.com/lightninglabs/darepo/clientconn"
	"github.com/lightninglabs/darepo/timeout"
	"github.com/lightningnetwork/lnd/fn/v2"
)

// ActorConfig contains the configuration parameters for the rounds actor.
type ActorConfig struct {
	// ChainParams are the Bitcoin network parameters.
	ChainParams *chaincfg.Params

	// Logger is used for logging.
	Logger btclog.Logger

	// Terms are the operator terms for the round.
	Terms *batch.Terms

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
	return timeout.ID(fmt.Sprintf("%s:%s", roundID, phase))
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

// Start initializes the actor. It creates a new live round FSM to accept
// registrations. In the future, it will also resume any existing rounds from
// storage.
func (a *Actor) Start(ctx context.Context) error {
	// TODO(elle): Load previous rounds from storage that still need to be
	// managed (e.g., rounds awaiting confirmation).

	round, err := a.newRoundFSM(ctx)
	if err != nil {
		return fmt.Errorf("unable to create new round FSM: %w", err)
	}

	a.currentRound = round
	a.rounds[round.RoundID] = round

	return nil
}

// Receive processes an actor message and returns a response. This is the main
// entry point for the actor.
func (a *Actor) Receive(ctx context.Context,
	msg ActorMsg) fn.Result[ActorResp] {

	switch m := msg.(type) {
	case *JoinRoundRequest:
		return a.handleJoinRoundRequest(ctx, m)

	case *TimeoutMsg:
		return a.handleTimeout(ctx, m)

	case *RoundMsg:
		return a.handleRoundEvent(ctx, m)

	default:
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

	fsmPrefix := roundID.LogPrefix()
	fsmLogger := a.cfg.Logger.WithPrefix(fsmPrefix)

	env := &Environment{
		RoundID:             roundID,
		Log:                 fsmLogger,
		ChainParams:         a.cfg.ChainParams,
		BoardingInputLocker: a.cfg.BoardingInputLocker,
		ChainSource:         a.cfg.ChainSource,
		Terms:               a.cfg.Terms,
	}

	fsmCfg := StateMachineCfg{
		InitialState: &CreatedState{},
		Env:          env,
		Logger:       a.log.WithPrefix(roundID.LogPrefix()),
	}
	fsm := protofsm.NewStateMachine(fsmCfg)
	fsm.Start(ctx)

	return &RoundFSM{
		FSM:     &fsm,
		RoundID: roundID,
	}, nil
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
