package rounds

import (
	"context"
	"fmt"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightningnetwork/lnd/fn/v2"
)

// ActorConfig contains the configuration parameters for the rounds actor.
type ActorConfig struct {
	// Logger is used for logging.
	Logger btclog.Logger
}

// Actor is the server rounds actor. It wraps the round FSM and manages its
// lifecycle. It tracks multiple concurrent rounds - one "current" round that
// accepts new registrations, and sealed rounds that are still being processed.
type Actor struct {
	// cfg contains all the configuration for this actor.
	cfg *ActorConfig

	log btclog.Logger
}

// NewActor creates a new server rounds actor with the provided configuration.
// It will check the rounds-store for any rounds that still need to be tracked
// and resume them. It will create a new "live" round that will accept new
// registrations.
func NewActor(cfg *ActorConfig) fn.Result[*Actor] {
	return fn.Ok(&Actor{
		cfg: cfg,
		log: cfg.Logger,
	})
}

// Start initializes the actor. It creates a new live round FSM to accept
// registrations. In the future, it will also resume any existing rounds from
// storage.
func (a *Actor) Start(ctx context.Context) error {
	// TODO: Create initial round FSM.
	return nil
}

// Receive processes an actor message and returns a response. This is the main
// entry point for the actor.
func (a *Actor) Receive(ctx context.Context,
	msg ActorMsg) fn.Result[ActorResp] {

	switch m := msg.(type) {

	default:
		return fn.Err[ActorResp](fmt.Errorf(
			"unknown message type: %T", m))
	}
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
		default:
			// Unknown outbox message. This could be an internal FSM
			// event that doesn't need routing, so we ignore it.
			_ = m
		}
	}

	return nil
}
