package oor

import (
	"context"
	"errors"
	"fmt"

	"github.com/btcsuite/btclog/v2"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

var (
	// errUnknownIncomingSession signals that no incoming session exists for
	// a requested session id.
	errUnknownIncomingSession = errors.New("unknown incoming session")
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

	// SessionStore persists outgoing session snapshots after each state
	// transition. This is optional and is primarily intended for mobile-style
	// restart/resume semantics.
	SessionStore OutgoingSessionStore

	// IncomingSessionStore persists incoming session snapshots after each state
	// transition. When configured, incoming sessions can be resumed without
	// explicit restore calls.
	IncomingSessionStore IncomingSessionStore
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

	// incomingSessions holds all currently active incoming sessions keyed by
	// session id.
	incomingSessions map[SessionID]*receiveSessionHandle
}

// NewOORClientActor creates a new outgoing-transfer OOR client actor.
func NewOORClientActor(cfg ClientActorCfg) *OORClientActor {
	if cfg.Logger == nil {
		cfg.Logger = btclog.Disabled
	}

	return &OORClientActor{
		cfg:              cfg,
		sessions:         make(map[SessionID]*sessionHandle),
		incomingSessions: make(map[SessionID]*receiveSessionHandle),
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

	case *RestoreSessionRequest:
		return a.handleRestoreSession(ctx, m)

	case *ResumeSessionRequest:
		return a.handleResumeSession(ctx, m)

	case *ExportSnapshotRequest:
		return a.handleExportSnapshot(ctx, m)

	case *ReceiveTransferRequest:
		return a.handleReceiveTransfer(ctx, m)

	case *ResumeIncomingRequest:
		return a.handleResumeIncoming(ctx, m)

	case *GetIncomingStateRequest:
		return a.handleGetIncomingState(ctx, m)

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

	handle := &sessionHandle{FSM: session.FSM}
	a.sessions[session.ID] = handle

	err = a.persistSession(ctx, session.ID, handle.FSM)
	if err != nil {
		return fn.Err[ActorResp](err)
	}

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

	handle, err := a.loadOutgoingHandle(ctx, req.SessionID)
	if err != nil {
		return fn.Err[ActorResp](err)
	}

	outbox, err := a.askEvent(ctx, handle.FSM, req.Event)
	if err != nil {
		return fn.Err[ActorResp](err)
	}

	err = a.persistSession(ctx, req.SessionID, handle.FSM)
	if err != nil {
		return fn.Err[ActorResp](err)
	}

	err = a.driveOutbox(ctx, req.SessionID, handle.FSM, outbox)
	if err != nil {
		return fn.Err[ActorResp](err)
	}

	return fn.Ok[ActorResp](&DriveEventResponse{})
}

// handleRestoreSession restores a session from an exported snapshot.
func (a *OORClientActor) handleRestoreSession(ctx context.Context,
	req *RestoreSessionRequest) fn.Result[ActorResp] {

	if req == nil {
		return fn.Err[ActorResp](fmt.Errorf("request must be provided"))
	}

	if req.Snapshot == nil {
		return fn.Err[ActorResp](
			fmt.Errorf("snapshot must be provided"),
		)
	}

	session, err := NewSessionFromSnapshot(ctx, req.Snapshot)
	if err != nil {
		return fn.Err[ActorResp](err)
	}

	a.sessions[session.ID] = &sessionHandle{FSM: session.FSM}

	err = a.persistSession(ctx, session.ID, session.FSM)
	if err != nil {
		return fn.Err[ActorResp](err)
	}

	return fn.Ok[ActorResp](&RestoreSessionResponse{
		SessionID: session.ID,
	})
}

// handleResumeSession re-emits the outbox implied by the session's current
// state.
func (a *OORClientActor) handleResumeSession(ctx context.Context,
	req *ResumeSessionRequest) fn.Result[ActorResp] {

	if req == nil {
		return fn.Err[ActorResp](fmt.Errorf("request must be provided"))
	}

	handle, err := a.loadOutgoingHandle(ctx, req.SessionID)
	if err != nil {
		return fn.Err[ActorResp](err)
	}

	state, err := handle.currentState()
	if err != nil {
		return fn.Err[ActorResp](err)
	}

	outbox, err := OutboxForState(state)
	if err != nil {
		return fn.Err[ActorResp](err)
	}

	err = a.driveOutbox(ctx, req.SessionID, handle.FSM, outbox)
	if err != nil {
		return fn.Err[ActorResp](err)
	}

	return fn.Ok[ActorResp](&ResumeSessionResponse{})
}

// handleExportSnapshot exports a snapshot for the requested session.
func (a *OORClientActor) handleExportSnapshot(ctx context.Context,
	req *ExportSnapshotRequest) fn.Result[ActorResp] {

	if req == nil {
		return fn.Err[ActorResp](fmt.Errorf("request must be provided"))
	}

	handle, err := a.loadOutgoingHandle(ctx, req.SessionID)
	if err != nil {
		return fn.Err[ActorResp](err)
	}

	state, err := handle.currentState()
	if err != nil {
		return fn.Err[ActorResp](err)
	}

	snapshot, err := NewOutgoingSnapshot(req.SessionID, state)
	if err != nil {
		return fn.Err[ActorResp](err)
	}

	return fn.Ok[ActorResp](&ExportSnapshotResponse{
		Snapshot: snapshot,
	})
}

// handleGetState returns the current state for the requested session.
func (a *OORClientActor) handleGetState(ctx context.Context,
	req *GetStateRequest) fn.Result[ActorResp] {

	if req == nil {
		return fn.Err[ActorResp](fmt.Errorf("request must be provided"))
	}

	handle, err := a.loadOutgoingHandle(ctx, req.SessionID)
	if err != nil {
		return fn.Err[ActorResp](err)
	}

	state, err := handle.currentState()
	if err != nil {
		return fn.Err[ActorResp](err)
	}

	return fn.Ok[ActorResp](&GetStateResponse{
		State: state,
	})
}

// handleReceiveTransfer processes an incoming transfer notification and drives
// any emitted outbox side effects.
func (a *OORClientActor) handleReceiveTransfer(ctx context.Context,
	req *ReceiveTransferRequest) fn.Result[ActorResp] {

	if req == nil {
		return fn.Err[ActorResp](fmt.Errorf("request must be provided"))
	}

	if req.SessionID == (SessionID{}) {
		return fn.Err[ActorResp](fmt.Errorf("session id must be provided"))
	}

	if req.ArkPSBT == nil || req.ArkPSBT.UnsignedTx == nil {
		return fn.Err[ActorResp](fmt.Errorf("ark psbt must be provided"))
	}

	handle, existing, err := a.getOrCreateIncomingHandle(ctx, req)
	if err != nil {
		return fn.Err[ActorResp](err)
	}

	var outbox []OutboxEvent

	// Existing incoming sessions are resumed from current durable state, so
	// duplicate notifications remain replay-safe and deterministic.
	if existing {
		state, err := handle.currentState()
		if err != nil {
			return fn.Err[ActorResp](err)
		}

		outbox, err = OutboxForReceiveState(state)
		if err != nil {
			return fn.Err[ActorResp](err)
		}
	} else {
		outbox, err = a.askEvent(ctx, handle.FSM, &IncomingTransferEvent{
			SessionID:            req.SessionID,
			ArkPSBT:              req.ArkPSBT,
			FinalCheckpointPSBTs: req.FinalCheckpointPSBTs,
		})
		if err != nil {
			return fn.Err[ActorResp](err)
		}

		err = a.persistIncomingSession(ctx, req.SessionID, handle.FSM)
		if err != nil {
			return fn.Err[ActorResp](err)
		}
	}

	err = a.driveIncomingOutbox(ctx, req.SessionID, handle.FSM, outbox)
	if err != nil {
		return fn.Err[ActorResp](err)
	}

	return fn.Ok[ActorResp](&ReceiveTransferResponse{})
}

// handleResumeIncoming re-emits incoming outbox work implied by the current
// session state.
func (a *OORClientActor) handleResumeIncoming(ctx context.Context,
	req *ResumeIncomingRequest) fn.Result[ActorResp] {

	if req == nil {
		return fn.Err[ActorResp](fmt.Errorf("request must be provided"))
	}

	handle, err := a.loadIncomingHandle(ctx, req.SessionID)
	if err != nil {
		return fn.Err[ActorResp](err)
	}

	state, err := handle.currentState()
	if err != nil {
		return fn.Err[ActorResp](err)
	}

	outbox, err := OutboxForReceiveState(state)
	if err != nil {
		return fn.Err[ActorResp](err)
	}

	err = a.driveIncomingOutbox(ctx, req.SessionID, handle.FSM, outbox)
	if err != nil {
		return fn.Err[ActorResp](err)
	}

	return fn.Ok[ActorResp](&ResumeIncomingResponse{})
}

// handleGetIncomingState returns the current state for the requested incoming
// session.
func (a *OORClientActor) handleGetIncomingState(ctx context.Context,
	req *GetIncomingStateRequest) fn.Result[ActorResp] {

	if req == nil {
		return fn.Err[ActorResp](fmt.Errorf("request must be provided"))
	}

	handle, err := a.loadIncomingHandle(ctx, req.SessionID)
	if err != nil {
		return fn.Err[ActorResp](err)
	}

	state, err := handle.currentState()
	if err != nil {
		return fn.Err[ActorResp](err)
	}

	return fn.Ok[ActorResp](&GetIncomingStateResponse{
		State: state,
	})
}

// loadOutgoingHandle returns the outgoing session handle from memory or durable
// snapshot state.
func (a *OORClientActor) loadOutgoingHandle(ctx context.Context,
	sessionID SessionID) (*sessionHandle, error) {

	if sessionID == (SessionID{}) {
		return nil, fmt.Errorf("session id must be provided")
	}

	handle, ok := a.sessions[sessionID]
	if ok {
		return handle, nil
	}

	store := a.cfg.SessionStore
	if store == nil {
		return nil, fmt.Errorf("unknown session: %s", sessionID)
	}

	snapshot, err := store.GetOutgoing(ctx, sessionID)
	if err != nil {
		if IsOutgoingSnapshotNotFound(err) {
			return nil, fmt.Errorf("unknown session: %s", sessionID)
		}

		return nil, err
	}

	session, err := NewSessionFromSnapshot(ctx, snapshot)
	if err != nil {
		return nil, err
	}

	handle = &sessionHandle{FSM: session.FSM}
	a.sessions[sessionID] = handle

	return handle, nil
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
		// Outbox handler is the I/O boundary.
		// Handler errors become OutboxErrorEvents for retry policy.
		followUps, err := handler.Handle(ctx, sessionID, msg)
		if err != nil {
			followUps = []Event{
				NewOutboxErrorEvent(msg, err),
			}
		}

		for _, followUp := range followUps {
			// Feed follow-up events into the FSM.
			// Recursively execute any emitted outbox work.
			// Stop when none remains.
			nextOutbox, err := a.askEvent(ctx, fsm, followUp)
			if err != nil {
				return err
			}

			// Persist after each transition.
			// This enables crash resume.
			//
			// This matters most after server co-sign.
			err = a.persistSession(ctx, sessionID, fsm)
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

// persistSession persists an outgoing session snapshot if a store is
// configured.
func (a *OORClientActor) persistSession(ctx context.Context,
	sessionID SessionID, fsm *StateMachine) error {

	store := a.cfg.SessionStore
	if store == nil {
		return nil
	}

	if fsm == nil {
		return fmt.Errorf("fsm must be provided")
	}

	current, err := fsm.CurrentState()
	if err != nil {
		return err
	}

	state, ok := current.(State)
	if !ok {
		return fmt.Errorf("unexpected state type: %T", current)
	}

	snapshot, err := NewOutgoingSnapshot(sessionID, state)
	if err != nil {
		return err
	}

	return store.UpsertOutgoing(ctx, snapshot)
}

// driveIncomingOutbox executes incoming outbox work and feeds follow-up events
// into the incoming FSM.
func (a *OORClientActor) driveIncomingOutbox(ctx context.Context,
	sessionID SessionID, fsm *StateMachine, outbox []OutboxEvent) error {

	handler := a.cfg.OutboxHandler
	if handler == nil {
		return nil
	}

	for _, msg := range outbox {
		followUps, err := handler.Handle(ctx, sessionID, msg)
		if err != nil {
			followUps = []Event{
				NewOutboxErrorEvent(msg, err),
			}
		}

		for _, followUp := range followUps {
			nextOutbox, err := a.askEvent(ctx, fsm, followUp)
			if err != nil {
				return err
			}

			err = a.persistIncomingSession(ctx, sessionID, fsm)
			if err != nil {
				return err
			}

			err = a.driveIncomingOutbox(
				ctx, sessionID, fsm, nextOutbox,
			)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

// persistIncomingSession persists an incoming session snapshot if a store is
// configured.
func (a *OORClientActor) persistIncomingSession(ctx context.Context,
	sessionID SessionID, fsm *StateMachine) error {

	store := a.cfg.IncomingSessionStore
	if store == nil {
		return nil
	}

	if fsm == nil {
		return fmt.Errorf("fsm must be provided")
	}

	current, err := fsm.CurrentState()
	if err != nil {
		return err
	}

	state, ok := current.(ReceiveState)
	if !ok {
		return fmt.Errorf("unexpected incoming state type: %T", current)
	}

	snapshot, err := NewIncomingSnapshot(sessionID, state)
	if err != nil {
		return err
	}

	return store.UpsertIncoming(ctx, snapshot)
}

// loadIncomingHandle returns the incoming session handle from memory or durable
// snapshot state.
func (a *OORClientActor) loadIncomingHandle(ctx context.Context,
	sessionID SessionID) (*receiveSessionHandle, error) {

	if sessionID == (SessionID{}) {
		return nil, fmt.Errorf("session id must be provided")
	}

	handle, ok := a.incomingSessions[sessionID]
	if ok {
		return handle, nil
	}

	store := a.cfg.IncomingSessionStore
	if store == nil {
		return nil, unknownIncomingSessionErr(sessionID)
	}

	snapshot, err := store.GetIncoming(ctx, sessionID)
	if err != nil {
		if IsIncomingSnapshotNotFound(err) {
			return nil, unknownIncomingSessionErr(sessionID)
		}

		return nil, err
	}

	session, err := NewReceiveSessionFromSnapshot(ctx, snapshot)
	if err != nil {
		return nil, err
	}

	handle = &receiveSessionHandle{FSM: session.FSM}
	a.incomingSessions[sessionID] = handle

	return handle, nil
}

// getOrCreateIncomingHandle returns an existing incoming session or creates a
// fresh one from the transfer request.
func (a *OORClientActor) getOrCreateIncomingHandle(ctx context.Context,
	req *ReceiveTransferRequest) (*receiveSessionHandle, bool, error) {

	handle, err := a.loadIncomingHandle(ctx, req.SessionID)
	if err == nil {
		return handle, true, nil
	}

	if !isUnknownIncomingSessionErr(err) {
		return nil, false, err
	}

	session, createErr := NewReceiveSession(
		ctx, req.ArkPSBT, req.SessionID,
	)
	if createErr != nil {
		return nil, false, createErr
	}

	handle = &receiveSessionHandle{FSM: session.FSM}
	a.incomingSessions[req.SessionID] = handle

	return handle, false, nil
}

// isUnknownIncomingSessionErr reports whether err is the actor-level unknown
// incoming-session error.
func isUnknownIncomingSessionErr(err error) bool {
	return errors.Is(err, errUnknownIncomingSession)
}

// unknownIncomingSessionErr returns a stable unknown-session error shape.
func unknownIncomingSessionErr(sessionID SessionID) error {
	return fmt.Errorf("%w: %s", errUnknownIncomingSession, sessionID)
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

// receiveSessionHandle ties an incoming session ID to its running state
// machine instance.
type receiveSessionHandle struct {
	FSM *StateMachine
}

// currentState returns the current concrete incoming session state.
func (h *receiveSessionHandle) currentState() (ReceiveState, error) {
	current, err := h.FSM.CurrentState()
	if err != nil {
		return nil, err
	}

	state, ok := current.(ReceiveState)
	if !ok {
		return nil, fmt.Errorf("unexpected incoming state type: %T",
			current)
	}

	return state, nil
}
