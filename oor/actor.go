package oor

import (
	"context"
	"fmt"

	"github.com/btcsuite/btclog/v2"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

// OutboxHandler executes FSM outbox requests and returns follow-up events.
//
// This mirrors the server-side OOR coordinator approach. The goal is to keep
// the FSM pure and move I/O (RPC, signing, persistence) behind an explicit
// boundary that can later be implemented by durable actors.
type OutboxHandler interface {
	// Handle executes the outbox request and returns follow-up events.
	Handle(ctx context.Context, sessionID SessionID,
		outbox OutboxEvent) ([]Event, error)
}

// ClientActorCfg configures the OORClientActor.
type ClientActorCfg struct {
	// Logger is used for actor and FSM logging.
	Logger btclog.Logger

	// OutboxHandler executes side effects emitted by the FSM.
	OutboxHandler OutboxHandler
}

// OORClientActor wraps the outgoing-transfer client FSM in an actor interface.
//
// The actor owns a set of per-session protofsm state machines and drives them
// by executing outbox requests via an OutboxHandler.
type OORClientActor struct {
	cfg ClientActorCfg

	// sessions holds all currently active transfer sessions keyed by the v0
	// session id (Ark txid).
	sessions map[SessionID]*sessionHandle
}

// NewOORClientActor creates a new outgoing-transfer OOR client actor.
func NewOORClientActor(cfg ClientActorCfg) *OORClientActor {
	if cfg.Logger == nil {
		cfg.Logger = btclog.Disabled
	}

	return &OORClientActor{
		cfg:      cfg,
		sessions: make(map[SessionID]*sessionHandle),
	}
}

// Receive processes a client actor message and returns a response.
func (a *OORClientActor) Receive(ctx context.Context,
	msg ActorMsg) fn.Result[ActorResp] {

	switch m := msg.(type) {
	case *StartTransferRequest:
		return a.handleStartTransfer(ctx, m)

	case *DriveEventRequest:
		return a.handleDriveEvent(ctx, m)

	case *GetStateRequest:
		return a.handleGetState(ctx, m)

	default:
		return fn.Err[ActorResp](fmt.Errorf("unknown message type: %T",
			m))
	}
}

// handleStartTransfer starts a new outgoing transfer session.
func (a *OORClientActor) handleStartTransfer(ctx context.Context,
	req *StartTransferRequest) fn.Result[ActorResp] {

	if req == nil {
		return fn.Err[ActorResp](fmt.Errorf("request must be provided"))
	}

	// Build the deterministic submit package and start the session FSM.
	// I/O is emitted as outbox messages.
	session, outbox, err := NewSession(
		ctx, req.Policy, req.Inputs, req.Recipients,
	)
	if err != nil {
		return fn.Err[ActorResp](err)
	}

	// StartTransferRequest is treated as idempotent: if the same
	// deterministic transfer is submitted twice (e.g. due to retries or
	// durable replay), we keep the existing session and return its ID.
	if _, exists := a.sessions[session.ID]; exists {
		return fn.Ok[ActorResp](&StartTransferResponse{
			SessionID: session.ID,
		})
	}

	handle := &sessionHandle{FSM: session.FSM}
	a.sessions[session.ID] = handle

	err = a.driveOutbox(ctx, session.ID, handle.FSM, outbox)
	if err != nil {
		return fn.Err[ActorResp](err)
	}

	return fn.Ok[ActorResp](&StartTransferResponse{
		SessionID: session.ID,
	})
}

// handleDriveEvent feeds a follow-up event into an existing session.
func (a *OORClientActor) handleDriveEvent(ctx context.Context,
	req *DriveEventRequest) fn.Result[ActorResp] {

	if req == nil {
		return fn.Err[ActorResp](fmt.Errorf("request must be provided"))
	}

	if req.Event == nil {
		return fn.Err[ActorResp](fmt.Errorf("event must be provided"))
	}

	handle, ok := a.sessions[req.SessionID]
	if !ok {
		return fn.Err[ActorResp](fmt.Errorf("unknown session: %s",
			req.SessionID))
	}

	outbox, err := a.askEvent(ctx, handle.FSM, req.Event)
	if err != nil {
		return fn.Err[ActorResp](err)
	}

	err = a.driveOutbox(ctx, req.SessionID, handle.FSM, outbox)
	if err != nil {
		return fn.Err[ActorResp](err)
	}

	return fn.Ok[ActorResp](&DriveEventResponse{})
}

// handleGetState returns the current state for the requested session.
func (a *OORClientActor) handleGetState(ctx context.Context,
	req *GetStateRequest) fn.Result[ActorResp] {

	_ = ctx

	if req == nil {
		return fn.Err[ActorResp](fmt.Errorf("request must be provided"))
	}

	handle, ok := a.sessions[req.SessionID]
	if !ok {
		return fn.Err[ActorResp](fmt.Errorf("unknown session: %s",
			req.SessionID))
	}

	state, err := handle.currentState()
	if err != nil {
		return fn.Err[ActorResp](err)
	}

	return fn.Ok[ActorResp](&GetStateResponse{
		State: state,
	})
}

// askEvent asks an event on the FSM and returns any outbox produced.
func (a *OORClientActor) askEvent(ctx context.Context, fsm *StateMachine,
	event Event) ([]OutboxEvent, error) {

	if fsm == nil {
		return nil, fmt.Errorf("fsm must be provided")
	}

	fut := fsm.AskEvent(ctx, event)
	result := fut.Await(ctx)
	if result.IsErr() {
		return nil, result.Err()
	}

	return result.UnwrapOr(nil), nil
}

// driveOutbox executes outbox work using the configured handler and feeds any
// follow-up events back into the FSM.
func (a *OORClientActor) driveOutbox(ctx context.Context, sessionID SessionID,
	fsm *StateMachine, outbox []OutboxEvent) error {

	handler := a.cfg.OutboxHandler
	if handler == nil {
		return nil
	}

	for _, msg := range outbox {
		// The outbox boundary is the only place where I/O is allowed.
		// The handler returns follow-up events for the FSM.
		followUps, err := handler.Handle(ctx, sessionID, msg)
		if err != nil {
			return err
		}

		for _, followUp := range followUps {
			// Feed follow-up events into the FSM.
			// Recursively execute any emitted outbox work.
			// Stop when none remains.
			nextOutbox, err := a.askEvent(ctx, fsm, followUp)
			if err != nil {
				return err
			}

			err = a.driveOutbox(ctx, sessionID, fsm, nextOutbox)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

// sessionHandle ties a session ID to its running state machine instance.
type sessionHandle struct {
	FSM *StateMachine
}

// currentState returns the current concrete OOR session state.
func (h *sessionHandle) currentState() (State, error) {
	current, err := h.FSM.CurrentState()
	if err != nil {
		return nil, err
	}

	state, ok := current.(State)
	if !ok {
		return nil, fmt.Errorf("unexpected state type: %T", current)
	}

	return state, nil
}
