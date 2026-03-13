package oor

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/baselib/protofsm"
	"github.com/lightninglabs/darepo-client/lib/scripts"
	oorlib "github.com/lightninglabs/darepo-client/lib/tx/oor"
	"github.com/lightninglabs/darepo/clientconn"
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

// ActorCfg configures the TransferCoordinatorActor.
type ActorCfg struct {
	// Log is an optional logger. When None, logging is disabled.
	Log fn.Option[btclog.Logger]

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

	// ClientsConn is an optional reference to the clientconn bridge for
	// pushing responses and incoming transfer notifications to clients.
	// When nil, responses are only returned via the actor's Ask path.
	ClientsConn actor.TellOnlyRef[clientconn.ClientConnMsg]

	// ActorID is the mailbox/checkpoint identifier for this coordinator.
	// Defaults to "oor.transfer.coordinator" if empty.
	ActorID string
}

// TransferCoordinatorActor manages concurrent OOR transfer session FSMs. Each
// session tracks one OOR transfer from submit through co-signing,
// finalization, and recipient notification.
//
// The actor implements ActorBehavior[OORDurableMsg, ActorResp] and is driven
// by a DurableActor runtime for crash-safe mailbox delivery. Each ActorMsg
// type implements TLVMessage directly (TLVType, Encode, Decode), so the
// durable mailbox serializes messages without an intermediate envelope layer.
//
// Responses are pushed to clients via ClientsConn (fire-and-forget) in
// addition to being returned from Receive for callers that use Ask.
type TransferCoordinatorActor struct {
	cfg ActorCfg

	actorID string

	// sessionsMu protects the sessions map. The durable runtime is
	// single-threaded, but CurrentState may be called from other
	// goroutines for observability.
	sessionsMu sync.RWMutex

	// sessions maps session IDs to running FSM instances. Sessions are
	// created on submit and removed after finalization or failure.
	sessions map[SessionID]*sessionHandle

	durable *actor.DurableActor[OORDurableMsg, ActorResp]
	ref     actor.ActorRef[OORDurableMsg, ActorResp]
}

// Compile-time check that TransferCoordinatorActor implements the durable
// actor behavior interface.
var _ actor.ActorBehavior[OORDurableMsg, ActorResp] = (
	*TransferCoordinatorActor)(nil)

// log returns the configured logger or a disabled fallback.
func (a *TransferCoordinatorActor) log() btclog.Logger {
	return a.cfg.Log.UnwrapOr(btclog.Disabled)
}

// Actor is a backward-compatible alias for TransferCoordinatorActor.
type Actor = TransferCoordinatorActor

// NewTransferCoordinatorActor creates a new coordinator actor instance.
//
// This is a pure constructor that performs no I/O. Call Start to initialize
// the durable runtime and begin processing.
func NewTransferCoordinatorActor(
	cfg ActorCfg) *TransferCoordinatorActor {

	actorID := cfg.ActorID
	if actorID == "" {
		actorID = defaultDurableActorID
	}

	return &TransferCoordinatorActor{
		cfg:      cfg,
		actorID:  actorID,
		sessions: make(map[SessionID]*sessionHandle),
	}
}

// NewActor is a backward-compatible alias for NewTransferCoordinatorActor.
func NewActor(cfg ActorCfg) *TransferCoordinatorActor {
	return NewTransferCoordinatorActor(cfg)
}

// Start loads durable mailbox state and starts the actor runtime.
//
// On restart the delivery store's checkpoint is used only to track the mailbox
// cursor. Active session state is rebuilt from the DB via
// SessionStore.LoadActiveSessions inside handleRestart.
func (a *TransferCoordinatorActor) Start(ctx context.Context) error {
	if a.cfg.DeliveryStore == nil {
		return fmt.Errorf("delivery store must be provided")
	}

	codec := newOORActorCodec()

	durableCfg := actor.DefaultDurableActorConfig[
		OORDurableMsg, ActorResp,
	](
		a.actorID, a, a.cfg.DeliveryStore, codec,
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
func (a *TransferCoordinatorActor) Stop() {
	if a.durable != nil {
		a.durable.Stop()
	}
}

// Ref returns the durable actor ref for Ask/Tell. Callers can send
// SubmitOORRequest or FinalizeOORRequest directly — each implements
// TLVMessage for transparent durable mailbox serialization.
func (a *TransferCoordinatorActor) Ref() actor.ActorRef[
	OORDurableMsg, ActorResp] {

	return a.ref
}

// CurrentState returns the current FSM state for the given session.
func (a *TransferCoordinatorActor) CurrentState(_ context.Context,
	sessionID SessionID) (State, error) {

	return a.currentState(sessionID)
}

// Receive processes one durable message. This is the ActorBehavior
// implementation called by the durable runtime. Session state changes are
// persisted through outbox side effects (session store, VTXO store) rather
// than actor checkpoint blobs.
func (a *TransferCoordinatorActor) Receive(ctx context.Context,
	msg OORDurableMsg) fn.Result[ActorResp] {

	switch m := msg.(type) {
	case *actor.RestartMessage:
		err := a.handleRestart(ctx, m)
		if err != nil {
			return fn.Err[ActorResp](err)
		}

		return fn.Ok[ActorResp](nil)

	case *SubmitOORRequest:
		resp, err := a.handleSubmit(ctx, m)
		if err != nil {
			return fn.Err[ActorResp](err)
		}

		return fn.Ok[ActorResp](resp)

	case *FinalizeOORRequest:
		resp, err := a.handleFinalize(ctx, m)
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
func (a *TransferCoordinatorActor) currentState(
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
func (a *TransferCoordinatorActor) handleSubmit(ctx context.Context,
	msg *SubmitOORRequest) (ActorResp, error) {

	if msg == nil || msg.ArkPSBT == nil || msg.ArkPSBT.UnsignedTx == nil {
		return nil, fmt.Errorf("ark psbt must be provided")
	}

	// Run submit validation before we touch lock state. Stateful/ownership
	// checks remain at the outbox boundary.
	validated, err := oorlib.ValidateSubmitPackageSigned(
		msg.ArkPSBT, msg.CheckpointPSBTs,
	)
	if err != nil {
		return nil, err
	}

	// Enforce static checkpoint policy values (operator key + CSV delay)
	// so malformed checkpoints fail before any session side effects.
	err = validateSubmitCheckpointPolicy(
		msg.ArkPSBT, a.cfg.CheckpointPolicy,
	)
	if err != nil {
		return nil, err
	}

	sessionID := SessionID(validated.ArkTxid)

	a.log().InfoS(ctx, "Processing submit request",
		btclog.Hex("session_id", sessionID[:]),
		slog.Int("num_checkpoints", len(msg.CheckpointPSBTs)),
		slog.Int("num_descs", len(msg.VTXOSigningDescriptors)))

	session, err := a.getOrCreateSessionFSM(ctx, sessionID)
	if err != nil {
		return nil, err
	}

	_, err = a.askAndDrive(ctx, sessionID, session.FSM,
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
		resp := &SubmitOORResponse{
			clientID:                msg.ClientID,
			SessionID:               sessionID,
			CoSignedCheckpointPSBTs: s.CoSignedCheckpointPSBTs,
		}

		a.log().InfoS(ctx, "Submit co-signed",
			btclog.Hex("session_id", sessionID[:]),
			slog.Int("num_co_signed", len(s.CoSignedCheckpointPSBTs)))

		// Push the response to the submitting client via clientconn
		// if configured. This is the primary delivery path for
		// bridge/RPC callers using Tell.
		a.pushClientResponse(ctx, resp)

		return resp, nil

	case *FailedState:
		// Clean up the session from the map now that it has
		// reached a terminal state.
		a.sessionsMu.Lock()
		delete(a.sessions, sessionID)
		a.sessionsMu.Unlock()

		a.log().DebugS(ctx, "Submit failed",
			btclog.Hex("session_id", sessionID[:]),
			slog.String("reason", s.Reason))

		return nil, fmt.Errorf("submit failed: %s", s.Reason)

	default:
		return nil, fmt.Errorf(
			"submit did not reach co-signed state: %T", state,
		)
	}
}

// handleFinalize drives an existing session FSM through finalize processing.
func (a *TransferCoordinatorActor) handleFinalize(ctx context.Context,
	msg *FinalizeOORRequest) (ActorResp, error) {

	if msg == nil {
		return nil, fmt.Errorf("request must be provided")
	}

	a.log().InfoS(ctx, "Processing finalize request",
		btclog.Hex("session_id", msg.SessionID[:]),
		slog.Int("num_checkpoints", len(msg.FinalCheckpointPSBTs)))

	a.sessionsMu.RLock()
	session, ok := a.sessions[msg.SessionID]
	a.sessionsMu.RUnlock()
	if !ok {
		return a.handleMissingFinalizeSession(ctx, msg)
	}

	_, err := a.askAndDrive(ctx, msg.SessionID, session.FSM,
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
		a.sessionsMu.Lock()
		delete(a.sessions, msg.SessionID)
		a.sessionsMu.Unlock()

		resp := &FinalizeOORResponse{
			clientID:  msg.ClientID,
			SessionID: msg.SessionID,
		}

		a.log().InfoS(ctx, "Session finalized and cleaned up",
			btclog.Hex("session_id", msg.SessionID[:]))

		// Push the response to the requesting client via clientconn
		// if configured.
		a.pushClientResponse(ctx, resp)

		return resp, nil

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
		a.sessionsMu.Lock()
		delete(a.sessions, msg.SessionID)
		a.sessionsMu.Unlock()

		a.log().DebugS(ctx, "Finalize failed",
			btclog.Hex("session_id", msg.SessionID[:]),
			slog.String("reason", s.Reason))

		return nil, fmt.Errorf("finalize failed: %s", s.Reason)

	default:
		return nil, fmt.Errorf(
			"finalize did not reach finalized state: %T", state,
		)
	}
}

// pushClientResponse sends a response message to a client via the clientconn
// bridge. This is best-effort: failures are logged but do not fail the FSM
// processing.
func (a *TransferCoordinatorActor) pushClientResponse(ctx context.Context,
	msg clientconn.ClientMessage) {

	if a.cfg.ClientsConn == nil {
		return
	}

	tellErr := a.cfg.ClientsConn.Tell(
		ctx, &clientconn.SendServerEventRequest{Message: msg},
	)
	if tellErr != nil {
		a.log().WarnS(ctx,
			"Failed to push OOR response via clientconn",
			tellErr,
			slog.String("client_id", string(msg.ClientID())),
		)
	}
}

// handleMissingFinalizeSession handles finalize requests for sessions that are
// no longer materialized in memory.
//
// This path supports idempotent finalize retries after terminal in-memory
// cleanup by consulting the durable session store.
func (a *TransferCoordinatorActor) handleMissingFinalizeSession(
	ctx context.Context,
	msg *FinalizeOORRequest) (ActorResp, error) {

	if a.cfg.SessionStore == nil {
		return nil, fmt.Errorf("unknown session: %s", msg.SessionID)
	}

	state, found, err := a.cfg.SessionStore.GetSessionState(
		ctx, msg.SessionID,
	)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("unknown session: %s", msg.SessionID)
	}

	switch state {
	case oorStateFinalized:
		pkg, err := a.cfg.SessionStore.LoadFinalizedPackage(
			ctx, msg.SessionID,
		)
		if err != nil {
			return nil, err
		}

		err = requireFinalCheckpointPackageMatch(
			pkg.FinalCheckpointPSBTs, msg.FinalCheckpointPSBTs,
		)
		if err != nil {
			return nil, err
		}

		resp := &FinalizeOORResponse{
			clientID:  msg.ClientID,
			SessionID: msg.SessionID,
		}

		a.pushClientResponse(ctx, resp)

		return resp, nil

	case oorStateAwaitingNotify:
		pkg, err := a.cfg.SessionStore.LoadFinalizedPackage(
			ctx, msg.SessionID,
		)
		if err != nil {
			return nil, err
		}

		err = requireFinalCheckpointPackageMatch(
			pkg.FinalCheckpointPSBTs, msg.FinalCheckpointPSBTs,
		)
		if err != nil {
			return nil, err
		}

		return nil, fmt.Errorf("notify recipients pending")

	default:
		return nil, fmt.Errorf("unknown session: %s", msg.SessionID)
	}
}

// getOrCreateSessionFSM returns an existing session FSM or creates one in the
// idle state.
func (a *TransferCoordinatorActor) getOrCreateSessionFSM(
	ctx context.Context,
	sessionID SessionID) (*sessionHandle, error) {

	a.sessionsMu.Lock()
	defer a.sessionsMu.Unlock()

	handle, ok := a.sessions[sessionID]
	if ok {
		return handle, nil
	}

	handle, err := a.createSessionFSM(ctx, sessionID, &IdleState{})
	if err != nil {
		return nil, err
	}

	a.sessions[sessionID] = handle

	return handle, nil
}

// createSessionFSM constructs and starts a per-session state machine instance.
func (a *TransferCoordinatorActor) createSessionFSM(ctx context.Context,
	sessionID SessionID, initial State) (*sessionHandle, error) {

	if initial == nil {
		return nil, fmt.Errorf("initial state must be provided")
	}

	env := &Environment{
		SessionID:        sessionID,
		CheckpointPolicy: a.cfg.CheckpointPolicy,
	}

	fsmLogger := a.cfg.Log.UnwrapOr(btclog.Disabled).WithPrefix(
		sessionID.LogPrefix(),
	)
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
func (a *TransferCoordinatorActor) askAndDrive(ctx context.Context,
	sessionID SessionID, fsm *StateMachine,
	event Event) ([]OutboxEvent, error) {

	if fsm == nil {
		return nil, fmt.Errorf("fsm must be provided")
	}

	handler := a.cfg.OutboxHandler
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
func (a *TransferCoordinatorActor) handleRestart(ctx context.Context,
	restart *actor.RestartMessage) error {

	if restart == nil {
		return fmt.Errorf("restart message must be provided")
	}

	a.sessionsMu.Lock()
	defer a.sessionsMu.Unlock()

	a.sessions = make(map[SessionID]*sessionHandle)

	if a.cfg.SessionStore == nil {
		return nil
	}

	active, err := a.cfg.SessionStore.LoadActiveSessions(ctx)
	if err != nil {
		return err
	}

	a.log().InfoS(ctx, "Restoring active OOR sessions",
		slog.Int("num_sessions", len(active)))

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
				FinalCheckpointPSBTs: session.
					CheckpointPSBTs,
			}

		default:
			continue
		}

		handle, err := a.createSessionFSM(
			ctx, session.SessionID, state,
		)
		if err != nil {
			return err
		}

		a.sessions[session.SessionID] = handle
	}

	return nil
}

// sessionHandle ties a session ID to its running state machine instance.
type sessionHandle struct {
	// FSM is the per-session state machine.
	FSM *StateMachine
}
