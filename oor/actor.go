package oor

import (
	"context"
	"fmt"
	"sort"

	"github.com/btcsuite/btclog/v2"
	"github.com/google/uuid"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

const (
	oorCheckpointStateType = "oor.outgoing.sessions"
	oorCheckpointVersion   = 1
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

	// DeliveryStore backs the durable actor mailbox/checkpoint operations.
	DeliveryStore actor.DeliveryStore

	// ActorID is the durable mailbox id used for this actor instance.
	// Re-using the same ActorID across restarts enables checkpoint restore.
	ActorID string
}

// OORClientActor wraps the outgoing-transfer client FSM in a durable actor
// interface.
//
// The actor owns a set of per-session protofsm state machines and drives them
// by executing outbox requests via an OutboxHandler.
type OORClientActor struct {
	cfg ClientActorCfg

	ref     actor.ActorRef[actor.TLVMessage, ActorResp]
	durable *actor.DurableActor[actor.TLVMessage, ActorResp]

	startupErr error
}

// NewOORClientActor creates a new outgoing-transfer OOR client actor.
func NewOORClientActor(cfg ClientActorCfg) *OORClientActor {
	if cfg.Logger == nil {
		cfg.Logger = btclog.Disabled
	}

	if cfg.ActorID == "" {
		cfg.ActorID = fmt.Sprintf("oor-client-%s", uuid.NewString())
	}

	actorRef := &OORClientActor{cfg: cfg}

	if cfg.DeliveryStore == nil {
		actorRef.startupErr = fmt.Errorf(
			"delivery store must be provided",
		)

		return actorRef
	}

	codec := actor.NewMessageCodec()
	codec.MustRegister(oorDurableCommandTLVType,
		func() actor.TLVMessage {
			return &durableActorCommandMessage{}
		},
	)
	codec.MustRegister(actor.RestartTLVType,
		func() actor.TLVMessage {
			return &actor.RestartMessage{}
		},
	)

	behavior := &oorDurableBehavior{
		cfg:      cfg,
		sessions: make(map[SessionID]*sessionHandle),
	}

	durableCfg := actor.DefaultDurableActorConfig[actor.TLVMessage,
		ActorResp](
		cfg.ActorID,
		behavior,
		cfg.DeliveryStore,
		codec,
	)

	durable := actor.NewDurableActor(durableCfg)
	actorRef.durable = durable
	actorRef.ref = durable.Ref()

	checkpoint, err := cfg.DeliveryStore.LoadCheckpoint(
		context.Background(), cfg.ActorID,
	)
	if err != nil {
		actorRef.startupErr = err
		return actorRef
	}

	err = actor.PrependRestartMessage(
		context.Background(),
		cfg.DeliveryStore,
		codec,
		cfg.ActorID,
		checkpoint,
	)
	if err != nil {
		actorRef.startupErr = err
		return actorRef
	}

	durable.Start()

	return actorRef
}

// Receive processes a client actor message and returns a response.
func (a *OORClientActor) Receive(ctx context.Context,
	msg ActorMsg) fn.Result[ActorResp] {

	if a.startupErr != nil {
		return fn.Err[ActorResp](a.startupErr)
	}

	if a.ref == nil {
		return fn.Err[ActorResp](
			fmt.Errorf("durable actor not initialized"),
		)
	}

	cmd, err := durableCommandFromActorMsg(msg)
	if err != nil {
		return fn.Err[ActorResp](err)
	}

	fut := a.ref.Ask(ctx, cmd)

	return fut.Await(ctx)
}

// Stop shuts down the underlying durable actor.
func (a *OORClientActor) Stop() {
	if a.durable != nil {
		a.durable.Stop()
	}
}

// oorDurableBehavior implements the durable actor behavior for the OOR
// client. It dispatches decoded TLV messages to per-session FSMs and
// persists a combined checkpoint after every state mutation.
type oorDurableBehavior struct {
	cfg ClientActorCfg

	sessions map[SessionID]*sessionHandle
}

// Receive dispatches decoded TLV messages to the appropriate handler
// method based on message type.
func (b *oorDurableBehavior) Receive(ctx context.Context,
	msg actor.TLVMessage) fn.Result[ActorResp] {

	switch m := msg.(type) {
	case *actor.RestartMessage:
		return b.handleRestart(ctx, m)

	case *durableActorCommandMessage:
		request, err := actorMsgFromDurableCommand(m)
		if err != nil {
			return fn.Err[ActorResp](err)
		}

		switch typedReq := request.(type) {
		case *StartTransferRequest:
			return b.handleStartTransfer(ctx, typedReq)

		case *DriveEventRequest:
			return b.handleDriveEvent(ctx, typedReq)

		case *GetStateRequest:
			return b.handleGetState(ctx, typedReq)

		case *RestoreSessionRequest:
			return b.handleRestoreSession(ctx, typedReq)

		case *ResumeSessionRequest:
			return b.handleResumeSession(ctx, typedReq)

		case *ExportSnapshotRequest:
			return b.handleExportSnapshot(ctx, typedReq)

		default:
			return fn.Err[ActorResp](
				fmt.Errorf(
					"unknown message type: %T",
					typedReq,
				),
			)
		}

	default:
		return fn.Err[ActorResp](fmt.Errorf("unknown message type: %T",
			m))
	}
}

// handleRestart restores all sessions from the durable checkpoint (if
// present) and re-drives their outbox side effects.
func (b *oorDurableBehavior) handleRestart(ctx context.Context,
	msg *actor.RestartMessage) fn.Result[ActorResp] {

	if msg == nil {
		return fn.Err[ActorResp](fmt.Errorf("restart message must be " +
			"provided"))
	}

	b.sessions = make(map[SessionID]*sessionHandle)

	if msg.HasCheckpoint() {
		checkpoint := msg.Checkpoint.UnsafeFromSome()

		err := b.restoreFromCheckpoint(ctx, checkpoint.StateData)
		if err != nil {
			return fn.Err[ActorResp](err)
		}
	}

	err := b.resumeRestoredSessions(ctx)
	if err != nil {
		return fn.Err[ActorResp](err)
	}

	err = b.persistCheckpoint(ctx)
	if err != nil {
		return fn.Err[ActorResp](err)
	}

	return fn.Ok[ActorResp](&DriveEventResponse{})
}

// handleStartTransfer starts a new outgoing transfer session.
func (b *oorDurableBehavior) handleStartTransfer(ctx context.Context,
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
	if _, exists := b.sessions[session.ID]; exists {
		return fn.Ok[ActorResp](&StartTransferResponse{
			SessionID: session.ID,
		})
	}

	handle := &sessionHandle{FSM: session.FSM}
	b.sessions[session.ID] = handle

	err = b.persistCheckpoint(ctx)
	if err != nil {
		return fn.Err[ActorResp](err)
	}

	err = b.driveOutbox(ctx, session.ID, handle.FSM, outbox)
	if err != nil {
		return fn.Err[ActorResp](err)
	}

	return fn.Ok[ActorResp](&StartTransferResponse{
		SessionID: session.ID,
	})
}

// handleDriveEvent feeds a follow-up event into an existing session.
func (b *oorDurableBehavior) handleDriveEvent(ctx context.Context,
	req *DriveEventRequest) fn.Result[ActorResp] {

	if req == nil {
		return fn.Err[ActorResp](fmt.Errorf("request must be provided"))
	}

	if req.Event == nil {
		return fn.Err[ActorResp](fmt.Errorf("event must be provided"))
	}

	if submitAccepted, ok := req.Event.(*SubmitAcceptedEvent); ok {
		err := validateSubmitAcceptedIdentity(
			req.SessionID, submitAccepted,
		)
		if err != nil {
			return fn.Err[ActorResp](err)
		}
	}

	handle, ok := b.sessions[req.SessionID]
	if !ok {
		return fn.Err[ActorResp](fmt.Errorf("unknown session: %s",
			req.SessionID))
	}

	outbox, err := b.askEvent(ctx, handle.FSM, req.Event)
	if err != nil {
		return fn.Err[ActorResp](err)
	}

	err = b.persistCheckpoint(ctx)
	if err != nil {
		return fn.Err[ActorResp](err)
	}

	err = b.driveOutbox(ctx, req.SessionID, handle.FSM, outbox)
	if err != nil {
		return fn.Err[ActorResp](err)
	}

	return fn.Ok[ActorResp](&DriveEventResponse{})
}

// handleRestoreSession restores a session from an exported snapshot.
func (b *oorDurableBehavior) handleRestoreSession(ctx context.Context,
	req *RestoreSessionRequest) fn.Result[ActorResp] {

	if req == nil {
		return fn.Err[ActorResp](fmt.Errorf("request must be provided"))
	}

	if req.Snapshot == nil {
		return fn.Err[ActorResp](
			fmt.Errorf("snapshot must be provided"),
		)
	}

	if _, exists := b.sessions[req.Snapshot.SessionID]; exists {
		return fn.Err[ActorResp](fmt.Errorf(
			"duplicate session id during restore: %s",
			req.Snapshot.SessionID,
		))
	}

	session, err := NewSessionFromSnapshot(ctx, req.Snapshot)
	if err != nil {
		return fn.Err[ActorResp](err)
	}

	b.sessions[session.ID] = &sessionHandle{FSM: session.FSM}

	err = b.persistCheckpoint(ctx)
	if err != nil {
		return fn.Err[ActorResp](err)
	}

	return fn.Ok[ActorResp](&RestoreSessionResponse{
		SessionID: session.ID,
	})
}

// handleResumeSession re-emits the outbox implied by the session's current
// state.
func (b *oorDurableBehavior) handleResumeSession(ctx context.Context,
	req *ResumeSessionRequest) fn.Result[ActorResp] {

	if req == nil {
		return fn.Err[ActorResp](fmt.Errorf("request must be provided"))
	}

	handle, ok := b.sessions[req.SessionID]
	if !ok {
		return fn.Err[ActorResp](fmt.Errorf("unknown session: %s",
			req.SessionID))
	}

	state, err := handle.currentState()
	if err != nil {
		return fn.Err[ActorResp](err)
	}

	outbox, err := OutboxForState(state)
	if err != nil {
		return fn.Err[ActorResp](err)
	}

	err = b.driveOutbox(ctx, req.SessionID, handle.FSM, outbox)
	if err != nil {
		return fn.Err[ActorResp](err)
	}

	return fn.Ok[ActorResp](&ResumeSessionResponse{})
}

// handleExportSnapshot exports a snapshot for the requested session.
func (b *oorDurableBehavior) handleExportSnapshot(ctx context.Context,
	req *ExportSnapshotRequest) fn.Result[ActorResp] {

	_ = ctx

	if req == nil {
		return fn.Err[ActorResp](fmt.Errorf("request must be provided"))
	}

	handle, ok := b.sessions[req.SessionID]
	if !ok {
		return fn.Err[ActorResp](fmt.Errorf("unknown session: %s",
			req.SessionID))
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
func (b *oorDurableBehavior) handleGetState(ctx context.Context,
	req *GetStateRequest) fn.Result[ActorResp] {

	_ = ctx

	if req == nil {
		return fn.Err[ActorResp](fmt.Errorf("request must be provided"))
	}

	handle, ok := b.sessions[req.SessionID]
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

// restoreFromCheckpoint decodes a TLV checkpoint blob and rebuilds
// per-session FSMs from the embedded outgoing snapshots.
func (b *oorDurableBehavior) restoreFromCheckpoint(ctx context.Context,
	raw []byte) error {

	_ = ctx

	if len(raw) == 0 {
		return nil
	}

	var checkpoint outgoingSessionsCheckpoint
	checkpoint, err := decodeOutgoingSessionsCheckpoint(raw)
	if err != nil {
		return err
	}

	if checkpoint.Version != oorCheckpointVersion {
		return fmt.Errorf("unknown checkpoint version: %d",
			checkpoint.Version)
	}

	for i := range checkpoint.Snapshots {
		snapshot := checkpoint.Snapshots[i]

		if _, exists := b.sessions[snapshot.SessionID]; exists {
			return fmt.Errorf(
				"duplicate session id in checkpoint: %s",
				snapshot.SessionID,
			)
		}

		session, err := NewSessionFromSnapshot(ctx, snapshot)
		if err != nil {
			return err
		}

		b.sessions[session.ID] = &sessionHandle{FSM: session.FSM}
	}

	return nil
}

// resumeRestoredSessions iterates all restored sessions in deterministic
// order and re-drives their outbox side effects so that in-flight
// transfers resume from where they left off.
func (b *oorDurableBehavior) resumeRestoredSessions(ctx context.Context) error {
	sessionIDs := make([]SessionID, 0, len(b.sessions))
	for sessionID := range b.sessions {
		sessionIDs = append(sessionIDs, sessionID)
	}

	sort.SliceStable(sessionIDs, func(i, j int) bool {
		return sessionIDs[i].String() < sessionIDs[j].String()
	})

	for i := range sessionIDs {
		sessionID := sessionIDs[i]
		handle := b.sessions[sessionID]

		state, err := handle.currentState()
		if err != nil {
			return err
		}

		outbox, err := OutboxForState(state)
		if err != nil {
			return err
		}

		err = b.driveOutbox(ctx, sessionID, handle.FSM, outbox)
		if err != nil {
			return err
		}
	}

	return nil
}

// askEvent asks an event on the FSM and returns any outbox produced.
func (b *oorDurableBehavior) askEvent(ctx context.Context, fsm *StateMachine,
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
func (b *oorDurableBehavior) driveOutbox(ctx context.Context,
	sessionID SessionID, fsm *StateMachine, outbox []OutboxEvent) error {

	handler := b.cfg.OutboxHandler
	if handler == nil {
		return nil
	}

	for _, msg := range outbox {
		// Outbox handler is the I/O boundary.
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
			nextOutbox, err := b.askEvent(ctx, fsm, followUp)
			if err != nil {
				return err
			}

			err = b.persistCheckpoint(ctx)
			if err != nil {
				return err
			}

			err = b.driveOutbox(ctx, sessionID, fsm, nextOutbox)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

// persistCheckpoint snapshots every active session into a single TLV
// blob and writes it to the durable delivery store.
func (b *oorDurableBehavior) persistCheckpoint(ctx context.Context) error {
	if b.cfg.DeliveryStore == nil {
		return fmt.Errorf("delivery store must be provided")
	}

	sessionIDs := make([]SessionID, 0, len(b.sessions))
	for sessionID := range b.sessions {
		sessionIDs = append(sessionIDs, sessionID)
	}

	sort.SliceStable(sessionIDs, func(i, j int) bool {
		return sessionIDs[i].String() < sessionIDs[j].String()
	})

	snapshots := make([]*OutgoingSnapshot, 0, len(sessionIDs))
	for i := range sessionIDs {
		sessionID := sessionIDs[i]
		handle := b.sessions[sessionID]

		state, err := handle.currentState()
		if err != nil {
			return err
		}

		snapshot, err := NewOutgoingSnapshot(sessionID, state)
		if err != nil {
			return err
		}

		snapshots = append(snapshots, snapshot)
	}

	raw, err := encodeOutgoingSessionsCheckpoint(outgoingSessionsCheckpoint{
		Version:   oorCheckpointVersion,
		Snapshots: snapshots,
	})
	if err != nil {
		return err
	}

	return b.cfg.DeliveryStore.SaveCheckpoint(ctx, actor.CheckpointParams{
		ActorID:   b.cfg.ActorID,
		StateType: oorCheckpointStateType,
		StateData: raw,
		Version:   oorCheckpointVersion,
	})
}

type outgoingSessionsCheckpoint struct {
	Version   int
	Snapshots []*OutgoingSnapshot
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

type durableBehaviorIface = actor.ActorBehavior[
	actor.TLVMessage, ActorResp,
]

var _ durableBehaviorIface = (*oorDurableBehavior)(nil)
