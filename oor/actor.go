package oor

import (
	"context"
	"fmt"
	"sync"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/baselib/protofsm"
	"github.com/lightninglabs/darepo-client/lib/scripts"
	oorlib "github.com/lightninglabs/darepo-client/lib/tx/oor"
	"github.com/lightningnetwork/lnd/fn/v2"
)

// OutboxHandler executes FSM outbox requests and returns zero or more follow-up
// inbox events to feed back into the FSM.
//
// The intent is to hide missing subsystems (wallet signing, DB persistence,
// VTXO locking) behind a narrow interface. The coordinator actor is then just a
// router/driver between the FSM and those subsystems.
type OutboxHandler interface {
	// Handle executes the outbox request and returns follow-up events.
	Handle(ctx context.Context, sessionID SessionID,
		outbox OutboxEvent) ([]Event, error)
}

// ActorCfg configures the OORTransferCoordinator actor.
type ActorCfg struct {
	// Logger is used for coordinator and FSM logging.
	Logger btclog.Logger

	// CheckpointPolicy is the expected operator policy for submitted
	// checkpoint transactions.
	CheckpointPolicy scripts.CheckpointPolicy

	// OutboxHandler executes outbox side effects.
	OutboxHandler OutboxHandler
}

// Actor is a minimal OORTransferCoordinator actor implementation.
//
// It owns a map of session ID to per-session protofsm state machines, and it
// drives the session FSM by executing outbox requests via an OutboxHandler.
type Actor struct {
	cfg ActorCfg

	// sessionsMu guards all access to sessions.
	sessionsMu sync.RWMutex
	sessions   map[SessionID]*sessionHandle
}

// NewActor creates a new coordinator actor instance.
func NewActor(cfg ActorCfg) *Actor {
	if cfg.Logger == nil {
		cfg.Logger = btclog.Disabled
	}

	return &Actor{
		cfg:      cfg,
		sessions: make(map[SessionID]*sessionHandle),
	}
}

// CurrentState returns the current FSM state for the given session.
func (a *Actor) CurrentState(ctx context.Context,
	sessionID SessionID) (State, error) {

	a.sessionsMu.RLock()
	handle, ok := a.sessions[sessionID]
	a.sessionsMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown session: %s", sessionID)
	}

	current, err := handle.FSM.CurrentState()
	if err != nil {
		return nil, err
	}

	state, ok := current.(State)
	if !ok {
		return nil, fmt.Errorf("unexpected state type: %T",
			current)
	}

	return state, nil
}

// Receive processes an actor message and returns a response.
func (a *Actor) Receive(ctx context.Context,
	msg ActorMsg) fn.Result[ActorResp] {

	switch m := msg.(type) {
	case *SubmitOORRequest:
		return a.handleSubmit(ctx, m)

	case *FinalizeOORRequest:
		return a.handleFinalize(ctx, m)

	default:
		return fn.Err[ActorResp](fmt.Errorf("unknown message type: %T",
			m))
	}
}

// handleSubmit processes a submit request by creating (or reusing) the session
// FSM and feeding it a SubmitRequestedEvent.
func (a *Actor) handleSubmit(ctx context.Context,
	req *SubmitOORRequest) fn.Result[ActorResp] {

	if req == nil || req.ArkPSBT == nil || req.ArkPSBT.UnsignedTx == nil {
		return fn.Err[ActorResp](fmt.Errorf("ark psbt must be " +
			"provided"))
	}

	// Run structural submit validation before we touch lock state.
	// Stateful/ownership checks remain at the outbox boundary.
	//
	validated, err := oorlib.ValidateSubmitPackage(
		req.ArkPSBT, req.CheckpointPSBTs,
	)
	if err != nil {
		return fn.Err[ActorResp](err)
	}

	// Enforce static checkpoint policy values (operator key + CSV delay) so
	// malformed checkpoints fail before any session side effects.
	err = validateSubmitCheckpointPolicy(
		req.ArkPSBT, a.cfg.CheckpointPolicy,
	)
	if err != nil {
		return fn.Err[ActorResp](err)
	}

	sessionID := SessionID(validated.ArkTxid)

	session, err := a.getOrCreateSessionFSM(ctx, sessionID)
	if err != nil {
		return fn.Err[ActorResp](err)
	}

	outbox, err := a.askAndDrive(ctx, sessionID, session.FSM,
		&SubmitRequestedEvent{
			ArkPSBT:         req.ArkPSBT,
			CheckpointPSBTs: req.CheckpointPSBTs,
		})
	if err != nil {
		return fn.Err[ActorResp](err)
	}

	_ = outbox

	return fn.Ok[ActorResp](&SubmitOORResponse{
		SessionID: sessionID,
	})
}

// handleFinalize processes a finalize request by feeding the session FSM a
// FinalizeRequestedEvent.
func (a *Actor) handleFinalize(ctx context.Context,
	req *FinalizeOORRequest) fn.Result[ActorResp] {

	if req == nil {
		return fn.Err[ActorResp](fmt.Errorf("request must be provided"))
	}

	a.sessionsMu.RLock()
	session, ok := a.sessions[req.SessionID]
	a.sessionsMu.RUnlock()
	if !ok {
		return fn.Err[ActorResp](fmt.Errorf("unknown session: %s",
			req.SessionID))
	}

	outbox, err := a.askAndDrive(ctx, req.SessionID, session.FSM,
		&FinalizeRequestedEvent{
			FinalCheckpointPSBTs: req.FinalCheckpointPSBTs,
		})
	if err != nil {
		return fn.Err[ActorResp](err)
	}

	_ = outbox

	return fn.Ok[ActorResp](&FinalizeOORResponse{
		SessionID: req.SessionID,
	})
}

// sessionHandle ties a session ID to its running state machine instance.
type sessionHandle struct {
	// FSM is the per-session state machine.
	FSM *StateMachine
}

// getOrCreateSessionFSM returns the session FSM handle for the given
// session ID.
func (a *Actor) getOrCreateSessionFSM(ctx context.Context,
	sessionID SessionID) (*sessionHandle, error) {

	a.sessionsMu.Lock()
	defer a.sessionsMu.Unlock()

	handle, ok := a.sessions[sessionID]
	if ok {
		return handle, nil
	}

	fsmLogger := a.cfg.Logger.WithPrefix(sessionID.LogPrefix())
	env := &Environment{
		SessionID: sessionID,
		Log:       fsmLogger,
	}

	fsmCfg := StateMachineCfg{
		InitialState: &IdleState{},
		Env:          env,
		Logger:       fsmLogger,
		ErrorReporter: &contextErrorReporter{
			log: fsmLogger,
		},
	}

	sm := protofsm.NewStateMachine(fsmCfg)
	sm.Start(ctx)

	handle = &sessionHandle{
		FSM: &sm,
	}

	a.sessions[sessionID] = handle

	return handle, nil
}

// askAndDrive asks an event on the given FSM and drives any outbox work using
// the configured OutboxHandler.
func (a *Actor) askAndDrive(ctx context.Context, sessionID SessionID,
	fsm *StateMachine, event Event) ([]OutboxEvent, error) {

	if fsm == nil {
		return nil, fmt.Errorf("fsm must be provided")
	}

	handler := a.cfg.OutboxHandler
	var allOutbox []OutboxEvent
	queue := []Event{event}

	for len(queue) > 0 {
		next := queue[0]
		queue = queue[1:]

		fut := fsm.AskEvent(ctx, next)
		result := fut.Await(ctx)
		if result.IsErr() {
			return nil, result.Err()
		}

		outbox := result.UnwrapOr(nil)
		if len(outbox) > 0 {
			allOutbox = append(allOutbox, outbox...)
		}

		if handler == nil {
			continue
		}

		for _, msg := range outbox {
			// The outbox boundary is responsible for side effects:
			// - VTXO locking and in-flight marking
			// - operator signing
			// - persistence of point-of-no-return snapshots
			// - recipient event log writes
			//
			// The handler returns follow-up inbox events.
			// This keeps the coordinator logic
			// deterministic and keeps retry/backoff in the state
			// machine instead of ad-hoc control flow.
			followUps, err := handler.Handle(ctx, sessionID, msg)
			if err != nil {
				return nil, err
			}

			if len(followUps) > 0 {
				queue = append(queue, followUps...)
			}
		}
	}

	return allOutbox, nil
}
