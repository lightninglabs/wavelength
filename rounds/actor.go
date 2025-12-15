package rounds

import (
	"context"
	"fmt"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/baselib/protofsm"
	"github.com/lightninglabs/darepo/clientconn"
	"github.com/lightningnetwork/lnd/fn/v2"
)

// ActorConfig contains the configuration parameters for the rounds actor.
type ActorConfig struct {
	// Logger is used for logging.
	Logger btclog.Logger

	// ClientsConn is a reference to the ClientsConnectionActor for sending
	// messages to registered clients.
	ClientsConn actor.TellOnlyRef[clientconn.ClientConnMsg]
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
		RoundID: roundID,
		Log:     fsmLogger,
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
