package oor

import (
	"context"
	"fmt"
	"sync"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/baselib/protofsm"
	"github.com/lightninglabs/darepo-client/lib/scripts"
	oorlib "github.com/lightninglabs/darepo-client/lib/tx/oor"
	"github.com/lightningnetwork/lnd/fn/v2"
)

const (
	defaultDurableActorID = "oor.transfer.coordinator"
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

	// DeliveryStore persists actor mailbox state/checkpoints.
	DeliveryStore actor.DeliveryStore

	// SessionStore provides DB-authoritative session state for restart
	// recovery. When set, handleRestart loads active sessions from the
	// database instead of relying on actor checkpoint blobs.
	SessionStore SessionStore

	// ActorID is the mailbox/checkpoint identifier for this coordinator.
	ActorID string
}

// Actor is a durable OORTransferCoordinator actor wrapper.
//
// It owns a map of session ID to per-session protofsm state machines, and it
// drives the session FSM by executing outbox requests via an OutboxHandler.
type Actor struct {
	cfg ActorCfg

	actorID string

	behavior *coordinatorBehavior

	durable *actor.DurableActor[actor.TLVMessage, ActorResp]
	ref     actor.ActorRef[actor.TLVMessage, ActorResp]
}

// NewActor creates a new coordinator actor instance.
//
// This is a pure constructor that performs no I/O. Call Start to initialize the
// durable runtime and begin processing.
func NewActor(cfg ActorCfg) *Actor {
	if cfg.Logger == nil {
		cfg.Logger = btclog.Disabled
	}

	actorID := cfg.ActorID
	if actorID == "" {
		actorID = defaultDurableActorID
	}

	behavior := newCoordinatorBehavior(cfg, actorID)

	return &Actor{
		cfg:      cfg,
		actorID:  actorID,
		behavior: behavior,
	}
}

// Start loads durable mailbox state and starts the actor runtime.
//
// On restart the delivery store's checkpoint is used only to track the mailbox
// cursor. Active session state is rebuilt from the DB via
// SessionStore.LoadActiveSessions inside handleRestart.
func (a *Actor) Start(ctx context.Context) error {
	if a.cfg.DeliveryStore == nil {
		return fmt.Errorf("delivery store must be provided")
	}

	codec := newActorCodec()

	durableCfg := actor.DefaultDurableActorConfig[
		actor.TLVMessage, ActorResp,
	](
		a.actorID, a.behavior, a.cfg.DeliveryStore, codec,
	)
	a.durable = actor.NewDurableActor(durableCfg)
	a.ref = a.durable.Ref()

	checkpoint, err := a.cfg.DeliveryStore.LoadCheckpoint(
		ctx, a.actorID,
	)
	if err != nil {
		return err
	}

	err = actor.PrependRestartMessage(
		ctx, a.cfg.DeliveryStore, codec, a.actorID, checkpoint,
	)
	if err != nil {
		return err
	}

	a.durable.Start()

	return nil
}

// Stop stops the durable coordinator actor.
func (a *Actor) Stop() {
	if a.durable != nil {
		a.durable.Stop()
	}
}

// CurrentState returns the current FSM state for the given session.
func (a *Actor) CurrentState(_ context.Context,
	sessionID SessionID) (State, error) {

	if a.behavior == nil {
		return nil, fmt.Errorf("actor not initialized")
	}

	return a.behavior.currentState(sessionID)
}

// Receive processes an actor message and returns a response.
func (a *Actor) Receive(ctx context.Context,
	msg ActorMsg) fn.Result[ActorResp] {

	if a.ref == nil {
		return fn.Err[ActorResp](
			fmt.Errorf("actor not started"),
		)
	}

	var durableMsg actor.TLVMessage

	switch m := msg.(type) {
	case *SubmitOORRequest:
		durableMsg = newSubmitDurableMessage(m)

	case *FinalizeOORRequest:
		durableMsg = newFinalizeDurableMessage(m)

	default:
		return fn.Err[ActorResp](
			fmt.Errorf("unknown message type: %T", m),
		)
	}

	result := a.ref.Ask(ctx, durableMsg).Await(ctx)
	if result.IsErr() {
		return fn.Err[ActorResp](result.Err())
	}

	return fn.Ok[ActorResp](result.UnwrapOr(nil))
}

// coordinatorBehavior contains the deterministic session FSM logic. The
// durable runtime drives this behavior with persisted inbox delivery.
type coordinatorBehavior struct {
	cfg ActorCfg

	actorID string

	sessionsMu sync.RWMutex
	sessions   map[SessionID]*sessionHandle
}

// newCoordinatorBehavior initializes the durable coordinator behavior state.
func newCoordinatorBehavior(cfg ActorCfg,
	actorID string) *coordinatorBehavior {

	return &coordinatorBehavior{
		cfg:      cfg,
		actorID:  actorID,
		sessions: make(map[SessionID]*sessionHandle),
	}
}

type coordinatorActorBehavior = actor.ActorBehavior[
	actor.TLVMessage, ActorResp,
]

var _ coordinatorActorBehavior = (*coordinatorBehavior)(nil)

// Receive processes one durable message. Session state changes are persisted
// through outbox side effects (session store, VTXO store) rather than actor
// checkpoint blobs.
func (b *coordinatorBehavior) Receive(ctx context.Context,
	msg actor.TLVMessage) fn.Result[ActorResp] {

	switch m := msg.(type) {
	case *actor.RestartMessage:
		err := b.handleRestart(ctx, m)
		if err != nil {
			return fn.Err[ActorResp](err)
		}

		return fn.Ok[ActorResp](nil)

	case *submitDurableMessage:
		resp, err := b.handleSubmit(ctx, m)
		if err != nil {
			return fn.Err[ActorResp](err)
		}

		return fn.Ok[ActorResp](resp)

	case *finalizeDurableMessage:
		resp, err := b.handleFinalize(ctx, m)
		if err != nil {
			return fn.Err[ActorResp](err)
		}

		return fn.Ok[ActorResp](resp)

	default:
		return fn.Err[ActorResp](
			fmt.Errorf("unknown message type: %T", msg),
		)
	}
}

// currentState returns the currently materialized FSM state for one session.
func (b *coordinatorBehavior) currentState(
	sessionID SessionID) (State, error) {

	b.sessionsMu.RLock()
	handle, ok := b.sessions[sessionID]
	b.sessionsMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown session: %s", sessionID)
	}

	current, err := handle.FSM.CurrentState()
	if err != nil {
		return nil, err
	}

	state, ok := current.(State)
	if !ok {
		return nil, fmt.Errorf("unexpected state type: %T", current)
	}

	return state, nil
}

// handleSubmit validates submit inputs and drives the session FSM through the
// submit flow.
//
// Validate structure and policy before touching any session or lock state. This
// is intentional DoS protection: malformed packages are rejected before any
// durable side effects.
func (b *coordinatorBehavior) handleSubmit(ctx context.Context,
	msg *submitDurableMessage) (ActorResp, error) {

	if msg == nil || msg.ArkPSBT == nil || msg.ArkPSBT.UnsignedTx == nil {
		return nil, fmt.Errorf("ark psbt must be provided")
	}

	// Run structural submit validation before we touch lock state.
	// Stateful/ownership checks remain at the outbox boundary.
	validated, err := oorlib.ValidateSubmitPackage(
		msg.ArkPSBT, msg.CheckpointPSBTs,
	)
	if err != nil {
		return nil, err
	}

	// Enforce static checkpoint policy values (operator key + CSV delay)
	// so malformed checkpoints fail before any session side effects.
	err = validateSubmitCheckpointPolicy(
		msg.ArkPSBT, b.cfg.CheckpointPolicy,
	)
	if err != nil {
		return nil, err
	}

	sessionID := SessionID(validated.ArkTxid)

	session, err := b.getOrCreateSessionFSM(ctx, sessionID)
	if err != nil {
		return nil, err
	}

	_, err = b.askAndDrive(ctx, sessionID, session.FSM,
		&SubmitRequestedEvent{
			ArkPSBT:                msg.ArkPSBT,
			CheckpointPSBTs:        msg.CheckpointPSBTs,
			VTXOSigningDescriptors: msg.VTXOSigningDescriptors,
		})
	if err != nil {
		return nil, err
	}

	current, err := session.FSM.CurrentState()
	if err != nil {
		return nil, err
	}

	state, ok := current.(State)
	if !ok {
		return nil, fmt.Errorf("unexpected state type: %T", current)
	}

	switch s := state.(type) {
	case *CoSignedState:
		return &SubmitOORResponse{
			SessionID:               sessionID,
			CoSignedCheckpointPSBTs: s.CoSignedCheckpointPSBTs,
		}, nil

	case *FailedState:
		return nil, fmt.Errorf("submit failed: %s", s.Reason)

	default:
		return nil, fmt.Errorf(
			"submit did not reach co-signed state: %T", state,
		)
	}
}

// handleFinalize drives an existing session FSM through finalize processing.
func (b *coordinatorBehavior) handleFinalize(ctx context.Context,
	msg *finalizeDurableMessage) (ActorResp, error) {

	if msg == nil {
		return nil, fmt.Errorf("request must be provided")
	}

	b.sessionsMu.RLock()
	session, ok := b.sessions[msg.SessionID]
	b.sessionsMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown session: %s", msg.SessionID)
	}

	_, err := b.askAndDrive(ctx, msg.SessionID, session.FSM,
		&FinalizeRequestedEvent{
			FinalCheckpointPSBTs: msg.FinalCheckpointPSBTs,
		})
	if err != nil {
		return nil, err
	}

	current, err := session.FSM.CurrentState()
	if err != nil {
		return nil, err
	}

	state, ok := current.(State)
	if !ok {
		return nil, fmt.Errorf("unexpected state type: %T", current)
	}

	switch s := state.(type) {
	case *FinalizedState:
		// Clean up the session from the map now that it has reached
		// a terminal state.
		b.sessionsMu.Lock()
		delete(b.sessions, msg.SessionID)
		b.sessionsMu.Unlock()

		return &FinalizeOORResponse{
			SessionID: msg.SessionID,
		}, nil

	case *AwaitingRecipientsNotifyState:
		if s.LastNotifyFailureReason != "" {
			return nil, fmt.Errorf(
				"notify recipients failed: %s",
				s.LastNotifyFailureReason,
			)
		}

		return nil, fmt.Errorf("notify recipients pending")

	case *FailedState:
		// Clean up the session from the map now that it has reached
		// a terminal state.
		b.sessionsMu.Lock()
		delete(b.sessions, msg.SessionID)
		b.sessionsMu.Unlock()

		return nil, fmt.Errorf("finalize failed: %s", s.Reason)

	default:
		return nil, fmt.Errorf(
			"finalize did not reach finalized state: %T", state,
		)
	}
}

// getOrCreateSessionFSM returns an existing session FSM or creates one in the
// idle state.
func (b *coordinatorBehavior) getOrCreateSessionFSM(ctx context.Context,
	sessionID SessionID) (*sessionHandle, error) {

	b.sessionsMu.Lock()
	defer b.sessionsMu.Unlock()

	handle, ok := b.sessions[sessionID]
	if ok {
		return handle, nil
	}

	handle, err := b.createSessionFSM(ctx, sessionID, &IdleState{})
	if err != nil {
		return nil, err
	}

	b.sessions[sessionID] = handle

	return handle, nil
}

// createSessionFSM constructs and starts a per-session state machine instance.
func (b *coordinatorBehavior) createSessionFSM(ctx context.Context,
	sessionID SessionID, initial State) (*sessionHandle, error) {

	if initial == nil {
		return nil, fmt.Errorf("initial state must be provided")
	}

	env := &Environment{
		SessionID: sessionID,
	}

	fsmLogger := b.cfg.Logger.WithPrefix(sessionID.LogPrefix())
	fsmCfg := StateMachineCfg{
		InitialState: initial,
		Env:          env,
		Logger:       fsmLogger,
		ErrorReporter: &contextErrorReporter{
			log: fsmLogger,
		},
	}

	sm := protofsm.NewStateMachine(fsmCfg)
	sm.Start(ctx)

	return &sessionHandle{
		FSM: &sm,
	}, nil
}

// askAndDrive runs one inbox event through the FSM and then exhausts all
// follow-up outbox/inbox hops until the queue is empty.
func (b *coordinatorBehavior) askAndDrive(ctx context.Context,
	sessionID SessionID, fsm *StateMachine,
	event Event) ([]OutboxEvent, error) {

	if fsm == nil {
		return nil, fmt.Errorf("fsm must be provided")
	}

	handler := b.cfg.OutboxHandler
	var allOutbox []OutboxEvent

	// queue is breadth-first across follow-up events so one durable
	// inbox message executes as one deterministic mini-workflow.
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

		for _, out := range outbox {
			followUps, err := handler.Handle(
				ctx, sessionID, out,
			)
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

// handleRestart rebuilds in-memory session FSMs from DB-authoritative state.
//
// Active sessions (cosigned and awaiting_notify) are loaded from the session
// store and materialized as running FSM instances. The durable mailbox replay
// then delivers any pending messages on top of this restored state.
func (b *coordinatorBehavior) handleRestart(ctx context.Context,
	restart *actor.RestartMessage) error {

	if restart == nil {
		return fmt.Errorf("restart message must be provided")
	}

	b.sessionsMu.Lock()
	defer b.sessionsMu.Unlock()

	b.sessions = make(map[SessionID]*sessionHandle)

	if b.cfg.SessionStore == nil {
		return nil
	}

	active, err := b.cfg.SessionStore.LoadActiveSessions(ctx)
	if err != nil {
		return err
	}

	for _, session := range active {
		var state State

		switch session.State {
		case oorStateCoSigned:
			state = &CoSignedState{
				Inputs:  session.Inputs,
				ArkPSBT: session.ArkPSBT,
				CoSignedCheckpointPSBTs: session.
					CheckpointPSBTs,
			}

		case oorStateAwaitingNotify:
			state = &AwaitingRecipientsNotifyState{
				ArkPSBT: session.ArkPSBT,
			}

		default:
			continue
		}

		handle, err := b.createSessionFSM(
			ctx, session.SessionID, state,
		)
		if err != nil {
			return err
		}

		b.sessions[session.SessionID] = handle
	}

	return nil
}

// sessionHandle ties a session ID to its running state machine instance.
type sessionHandle struct {
	// FSM is the per-session state machine.
	FSM *StateMachine
}
