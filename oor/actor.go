package oor

import (
	"context"
	"fmt"
	"log/slog"
	"sort"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btclog/v2"
	"github.com/google/uuid"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/serverconn"
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

	// ServerConn is a reference to the ServerConnectionActor for sending
	// transport events (submit, finalize, ack) to the server. When set,
	// transport outbox events bypass the OutboxHandler and are Tell'd to
	// the connection actor for durable delivery. When nil, all outbox
	// events are routed through OutboxHandler for backward compatibility.
	ServerConn actor.TellOnlyRef[serverconn.ServerConnMsg]

	// PackageStore persists finalized outgoing packages and local input
	// bindings used by unroll/recovery tooling.
	PackageStore PackagePersistence

	// DeliveryStore backs the durable actor mailbox/checkpoint operations.
	DeliveryStore actor.DeliveryStore

	// ActorSystem is the system in which the OOR actor registers itself
	// under the OOR service key. This enables serverconn ingress
	// dispatching and timeout callback wiring via service key lookup.
	// When nil, the actor is not registered (useful for unit tests).
	ActorSystem actor.SystemContext

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

	ref     actor.ActorRef[OORDurableMsg, ActorResp]
	durable *actor.DurableActor[OORDurableMsg, ActorResp]

	startupErr error
}

// newOORActorCodec creates a MessageCodec with all OOR actor message types
// registered. This allows the durable actor to serialize and deserialize each
// ActorMsg type directly without an intermediate envelope.
//
// IMPORTANT: every type that implements ActorMsg must be registered here;
// omissions cause runtime dispatch failures with no compile-time warning.
func newOORActorCodec() *actor.MessageCodec {
	codec := actor.NewMessageCodec()

	codec.MustRegister(
		StartTransferRequestTLVType,
		func() actor.TLVMessage {
			return &StartTransferRequest{}
		},
	)
	codec.MustRegister(
		DriveEventRequestTLVType,
		func() actor.TLVMessage {
			return &DriveEventRequest{}
		},
	)
	codec.MustRegister(
		GetStateRequestTLVType,
		func() actor.TLVMessage {
			return &GetStateRequest{}
		},
	)
	codec.MustRegister(
		RestoreSessionRequestTLVType,
		func() actor.TLVMessage {
			return &RestoreSessionRequest{}
		},
	)
	codec.MustRegister(
		ResumeSessionRequestTLVType,
		func() actor.TLVMessage {
			return &ResumeSessionRequest{}
		},
	)
	codec.MustRegister(
		ExportSnapshotRequestTLVType,
		func() actor.TLVMessage {
			return &ExportSnapshotRequest{}
		},
	)
	codec.MustRegister(
		actor.RestartTLVType,
		func() actor.TLVMessage {
			return &actor.RestartMessage{}
		},
	)

	return codec
}

// NewOORClientActor creates a durable outgoing-transfer OOR client actor.
//
// Startup performs checkpoint loading and prepends a restart message so
// recovery logic runs through the same behavior path as normal runtime
// messages. If startup prerequisites fail, the returned actor stores the error
// and surfaces it on Receive.
func NewOORClientActor(cfg ClientActorCfg) *OORClientActor {
	if cfg.Logger == nil {
		cfg.Logger = btclog.Disabled
	}

	if cfg.ActorID == "" {
		cfg.ActorID = fmt.Sprintf("oor-client-%s", uuid.NewString())
	}

	cfg.Logger.InfoS(context.Background(), "Creating OOR client actor",
		slog.String("actor_id", cfg.ActorID))

	actorRef := &OORClientActor{cfg: cfg}

	if cfg.DeliveryStore == nil {
		actorRef.startupErr = fmt.Errorf(
			"delivery store must be provided",
		)

		return actorRef
	}

	codec := newOORActorCodec()

	behavior := &oorDurableBehavior{
		cfg:      cfg,
		log:      cfg.Logger,
		sessions: make(map[SessionID]*sessionHandle),
	}

	durableCfg := actor.DefaultDurableActorConfig[OORDurableMsg,
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

	cfg.Logger.InfoS(context.Background(), "OOR durable actor started",
		slog.String("actor_id", cfg.ActorID))

	// Register the durable actor's ref with the actor system so the
	// serverconn event router can discover it via the OOR service key.
	if cfg.ActorSystem != nil {
		oorKey := NewServiceKey()
		err = actor.RegisterWithReceptionist(
			cfg.ActorSystem.Receptionist(), oorKey,
			durable.Ref(),
		)
		if err != nil {
			actorRef.startupErr = fmt.Errorf(
				"register OOR actor: %w", err,
			)

			return actorRef
		}

		cfg.Logger.InfoS(
			context.Background(),
			"OOR actor registered with receptionist",
			slog.String("actor_id", cfg.ActorID),
		)
	}

	return actorRef
}

// Receive sends an actor message through the durable mailbox and returns
// the response synchronously. Each ActorMsg type implements TLVMessage
// directly, so no envelope conversion is needed.
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

	if msg == nil {
		return fn.Err[ActorResp](
			fmt.Errorf("message must be provided"),
		)
	}

	fut := a.ref.Ask(ctx, msg)

	return fut.Await(ctx)
}

// Stop shuts down the underlying durable actor and releases its goroutines.
//
// Stop is safe to call multiple times.
func (a *OORClientActor) Stop() {
	a.cfg.Logger.InfoS(
		context.Background(), "Stopping OOR client actor",
		slog.String("actor_id", a.cfg.ActorID),
	)

	if a.durable != nil {
		a.durable.Stop()
	}

	a.cfg.Logger.InfoS(
		context.Background(), "OOR client actor stopped",
		slog.String("actor_id", a.cfg.ActorID),
	)
}

// oorDurableBehavior implements the durable actor behavior for the OOR
// client. It dispatches decoded TLV messages to per-session FSMs and
// persists a combined checkpoint after every state mutation.
type oorDurableBehavior struct {
	cfg ClientActorCfg

	// log is a convenience reference to the configured logger, used for
	// structured logging throughout the behavior's message handlers.
	log btclog.Logger

	sessions map[SessionID]*sessionHandle
}

// logger returns the behavior's configured logger, falling back to the
// package-level logger if none was set. This guards against nil dereferences
// when tests construct the behavior directly without a Logger.
func (b *oorDurableBehavior) logger() btclog.Logger {
	if b.log != nil {
		return b.log
	}

	return log
}

// Receive dispatches decoded TLV messages to the appropriate handler
// method based on message type. Each ActorMsg type is registered directly
// in the codec and deserialized by the durable actor, so no envelope
// unwrapping is needed.
func (b *oorDurableBehavior) Receive(ctx context.Context,
	msg OORDurableMsg) fn.Result[ActorResp] {

	switch m := msg.(type) {
	case *actor.RestartMessage:
		return b.handleRestart(ctx, m)

	case *StartTransferRequest:
		return b.handleStartTransfer(ctx, m)

	case *DriveEventRequest:
		return b.handleDriveEvent(ctx, m)

	case *GetStateRequest:
		return b.handleGetState(ctx, m)

	case *RestoreSessionRequest:
		return b.handleRestoreSession(ctx, m)

	case *ResumeSessionRequest:
		return b.handleResumeSession(ctx, m)

	case *ExportSnapshotRequest:
		return b.handleExportSnapshot(ctx, m)

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

	hasCheckpoint := msg.HasCheckpoint()

	b.logger().InfoS(ctx, "Handling restart message",
		slog.Bool("has_checkpoint", hasCheckpoint))

	if hasCheckpoint {
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

	b.logger().InfoS(ctx, "Restart complete",
		slog.Int("num_sessions", len(b.sessions)))

	return fn.Ok[ActorResp](&DriveEventResponse{})
}

// handleStartTransfer starts a new outgoing transfer session.
func (b *oorDurableBehavior) handleStartTransfer(ctx context.Context,
	req *StartTransferRequest) fn.Result[ActorResp] {

	if req == nil {
		return fn.Err[ActorResp](fmt.Errorf("request must be provided"))
	}

	b.logger().InfoS(ctx, "Starting new OOR transfer",
		slog.Int("num_inputs", len(req.Inputs)),
		slog.Int("num_recipients", len(req.Recipients)))

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
		//nolint:ll
		b.logger().DebugS(ctx, "Duplicate start transfer, returning existing session",
			slog.String("session_id", session.ID.String()))

		return fn.Ok[ActorResp](&StartTransferResponse{
			SessionID: session.ID,
		})
	}

	handle := &sessionHandle{FSM: session.FSM}
	b.sessions[session.ID] = handle

	b.logger().InfoS(ctx, "OOR session created",
		slog.String("session_id", session.ID.String()),
		slog.Int("num_outbox", len(outbox)))

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

	b.logger().DebugS(ctx, "Driving event into session",
		slog.String("session_id", req.SessionID.String()),
		slog.String("event_type", fmt.Sprintf("%T", req.Event)))

	handle, ok := b.sessions[req.SessionID]
	if !ok {
		return fn.Err[ActorResp](fmt.Errorf("unknown session: %s",
			req.SessionID))
	}

	// If the inbound SubmitAcceptedEvent is missing the ArkPSBT (e.g.,
	// the server response proto does not echo it back), enrich from the
	// current session state. The AwaitingSubmitAccepted state carries
	// the canonical ArkPSBT that was sent in the submit request.
	if submitAccepted, ok := req.Event.(*SubmitAcceptedEvent); ok {
		if submitAccepted.ArkPSBT == nil {
			err := b.enrichSubmitAcceptedArkPSBT(
				handle, submitAccepted,
			)
			if err != nil {
				return fn.Err[ActorResp](err)
			}
		}

		err := validateSubmitAcceptedIdentity(
			req.SessionID, submitAccepted,
		)
		if err != nil {
			return fn.Err[ActorResp](err)
		}
	}

	finalizeState, err := b.captureFinalizeStateForEvent(
		handle.FSM, req.Event,
	)
	if err != nil {
		return fn.Err[ActorResp](err)
	}

	outbox, err := b.askEvent(ctx, handle.FSM, req.Event)
	if err != nil {
		return fn.Err[ActorResp](err)
	}

	if finalizeState != nil {
		err := b.persistOutgoingPackage(
			ctx, req.SessionID, finalizeState,
		)
		if err != nil {
			return fn.Err[ActorResp](err)
		}
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

// enrichSubmitAcceptedArkPSBT populates a SubmitAcceptedEvent's ArkPSBT field
// from the current session state when the server response does not echo it
// back. The canonical ArkPSBT lives in the AwaitingSubmitAccepted state, which
// was set when the client built and sent the submit package. This allows the
// dispatch adapter to construct a SubmitAcceptedEvent from the oorpb proto
// (which only carries sessionID + co-signed checkpoints) and have the actor
// enrich it before validation and transition processing.
func (b *oorDurableBehavior) enrichSubmitAcceptedArkPSBT(
	handle *sessionHandle,
	event *SubmitAcceptedEvent) error {

	state, err := handle.currentState()
	if err != nil {
		return fmt.Errorf("get current state for ArkPSBT "+
			"enrichment: %w", err)
	}

	awaitingSubmit, ok := state.(*AwaitingSubmitAccepted)
	if !ok {
		return fmt.Errorf("expected AwaitingSubmitAccepted "+
			"state for ArkPSBT enrichment, got %T", state)
	}

	event.ArkPSBT = awaitingSubmit.ArkPSBT

	return nil
}

// persistOutgoingPackage stores finalized outgoing package artifacts and input
// bindings for unroll/recovery lookup.
func (b *oorDurableBehavior) persistOutgoingPackage(ctx context.Context,
	sessionID SessionID, state *AwaitingFinalizeAccepted) error {

	if b.cfg.PackageStore == nil || state == nil {
		return nil
	}

	sessionHash := chainhash.Hash(sessionID)

	b.logger().DebugS(ctx, "Persisting outgoing package",
		slog.String("session_id", sessionID.String()),
		slog.Int("num_inputs", len(state.InputOutpoints)),
		slog.Int("num_checkpoints", len(state.FinalCheckpointPSBTs)))

	err := b.cfg.PackageStore.UpsertPackage(ctx,
		PackageDirectionOutgoing, sessionHash, state.ArkPSBT,
		state.FinalCheckpointPSBTs,
	)
	if err != nil {
		return err
	}

	for i := range state.InputOutpoints {
		err := b.cfg.PackageStore.UpsertBinding(ctx,
			state.InputOutpoints[i], sessionHash, uint32(i),
			PackageLinkKindConsumedInput,
		)
		if err != nil {
			return err
		}
	}

	return nil
}

// captureFinalizeStateForEvent snapshots finalize-state context before
// applying a follow-up event.
func (b *oorDurableBehavior) captureFinalizeStateForEvent(fsm *StateMachine,
	event Event) (*AwaitingFinalizeAccepted, error) {

	if b.cfg.PackageStore == nil {
		return nil, nil
	}

	if _, ok := event.(*FinalizeAcceptedEvent); !ok {
		return nil, nil
	}

	current, err := fsm.CurrentState()
	if err != nil {
		return nil, err
	}

	state, ok := current.(State)
	if !ok {
		return nil, fmt.Errorf("unexpected state type: %T", current)
	}

	finalizeState, ok := state.(*AwaitingFinalizeAccepted)
	if !ok {
		return nil, nil
	}

	return finalizeState, nil
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

	b.logger().InfoS(ctx, "Restoring session from snapshot",
		slog.String("session_id", req.Snapshot.SessionID.String()))

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

	b.logger().InfoS(ctx, "Session restored successfully",
		slog.String("session_id", session.ID.String()))

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

	b.logger().InfoS(ctx, "Resuming session",
		slog.String("session_id", req.SessionID.String()))

	handle, ok := b.sessions[req.SessionID]
	if !ok {
		return fn.Err[ActorResp](fmt.Errorf("unknown session: %s",
			req.SessionID))
	}

	state, err := handle.currentState()
	if err != nil {
		return fn.Err[ActorResp](err)
	}

	b.logger().DebugS(ctx, "Session current state for resume",
		slog.String("session_id", req.SessionID.String()),
		slog.String("state", fmt.Sprintf("%T", state)))

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

	b.logger().InfoS(ctx, "Restoring sessions from checkpoint",
		slog.Int("checkpoint_version", checkpoint.Version),
		slog.Int("num_snapshots", len(checkpoint.Snapshots)))

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

		b.logger().DebugS(ctx, "Restored session from checkpoint",
			slog.String("session_id", session.ID.String()))
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

	b.logger().InfoS(ctx, "Resuming restored sessions",
		slog.Int("num_sessions", len(sessionIDs)))

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

		b.logger().DebugS(ctx, "Resuming restored session",
			slog.String("session_id", sessionID.String()),
			slog.String("state", fmt.Sprintf("%T", state)),
			slog.Int("num_outbox", len(outbox)))

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

// Compile-time assertions that local outbox types satisfy
// serverconn.ServerMessage. These exist to document why isTransportEvent uses
// an explicit type switch rather than an interface assertion: if these types
// were ever routed to the server, it would cause fund-loss (inputs not marked
// spent locally) or liveness failure (retry timers lost).
var _ serverconn.ServerMessage = (*MarkInputsSpentRequest)(nil)
var _ serverconn.ServerMessage = (*ScheduleRetryRequest)(nil)

// isTransportEvent reports whether the outbox event should be routed to the
// server via serverconn rather than handled locally. This uses an explicit type
// switch instead of a serverconn.ServerMessage assertion because some local
// outbox types (MarkInputsSpentRequest, ScheduleRetryRequest) also satisfy
// that interface via their ToProto methods.
func (b *oorDurableBehavior) isTransportEvent(msg OutboxEvent) bool {
	if b.cfg.ServerConn == nil {
		return false
	}

	switch msg.(type) {
	case *SendSubmitPackageRequest, *SendFinalizePackageRequest,
		*SendIncomingAckRequest:

		return true

	default:
		return false
	}
}

// sendTransportEvent wraps the outbox message in a SendClientEventRequest and
// Tell's it to the serverconn actor for durable delivery to the server.
func (b *oorDurableBehavior) sendTransportEvent(ctx context.Context,
	msg OutboxEvent) error {

	serverMsg, ok := msg.(serverconn.ServerMessage)
	if !ok {
		return fmt.Errorf("transport event %T does not implement "+
			"ServerMessage", msg)
	}

	sendReq := &serverconn.SendClientEventRequest{
		Message: serverMsg,
	}

	if err := b.cfg.ServerConn.Tell(ctx, sendReq); err != nil {
		return fmt.Errorf("send transport event to server: %w", err)
	}

	return nil
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
		// Transport events (submit, finalize, ack) are Tell'd to
		// the serverconn actor for durable delivery. The FSM stays
		// in its AwaitingX state until the server response arrives
		// asynchronously via DriveEventRequest.
		if b.isTransportEvent(msg) {
			//nolint:ll
			b.logger().DebugS(ctx, "Sending transport event to server",
				slog.String("session_id", sessionID.String()),
				slog.String("event_type", fmt.Sprintf("%T", msg)))

			if err := b.sendTransportEvent(ctx, msg); err != nil {
				return err
			}

			continue
		}

		b.logger().DebugS(ctx, "Handling local outbox event",
			slog.String("session_id", sessionID.String()),
			slog.String("event_type", fmt.Sprintf("%T", msg)))

		// Local events (signing, persistence, timers) continue
		// through the outbox handler.
		followUps, err := handler.Handle(ctx, sessionID, msg)
		if err != nil {
			//nolint:ll
			b.logger().WarnS(ctx, "Outbox handler error, wrapping as retryable event", err,
				slog.String("session_id", sessionID.String()),
				slog.String("event_type", fmt.Sprintf("%T", msg)))

			followUps = []Event{
				NewOutboxErrorEvent(msg, err),
			}
		}

		for _, followUp := range followUps {
			finalizeState, err := b.captureFinalizeStateForEvent(
				fsm, followUp,
			)
			if err != nil {
				return err
			}

			// Feed follow-up events into the FSM.
			// Recursively execute any emitted outbox work.
			// Stop when none remains.
			nextOutbox, err := b.askEvent(ctx, fsm, followUp)
			if err != nil {
				return err
			}

			if finalizeState != nil {
				err = b.persistOutgoingPackage(ctx, sessionID,
					finalizeState)
				if err != nil {
					return err
				}
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
	OORDurableMsg, ActorResp,
]

var _ durableBehaviorIface = (*oorDurableBehavior)(nil)
