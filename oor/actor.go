package oor

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btclog/v2"
	"github.com/google/uuid"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/build"
	"github.com/lightninglabs/darepo-client/ledger"
	libtypes "github.com/lightninglabs/darepo-client/lib/types"
	"github.com/lightninglabs/darepo-client/serverconn"
	"github.com/lightninglabs/darepo-client/vtxo"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

const (
	oorCheckpointStateType = "oor.sessions"
	oorCheckpointVersion   = 2

	// incomingMetadataQueryLimit is the default page size for durable
	// ListVTXOsByScripts lookups used by the receive FSM.
	incomingMetadataQueryLimit uint32 = 128
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

type incomingMetadataFilter = IncomingMetadataRecipientFilter

// ClientActorCfg configures the OORClientActor.
type ClientActorCfg struct {
	// Log is an optional logger for this actor instance. If None, the
	// actor falls back to extracting a logger from context via
	// LoggerFromContext, or uses btclog.Disabled if no logger is found.
	Log fn.Option[btclog.Logger]

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

	// VTXOManager receives notifications after incoming VTXOs are durably
	// materialized so it can spawn VTXO actors for monitoring.
	VTXOManager actor.TellOnlyRef[vtxo.ManagerMsg]

	// VTXOStore reloads durably materialized incoming VTXOs
	// by outpoint when a callback event is restored from the
	// mailbox without in-memory descriptor attachments.
	VTXOStore vtxo.VTXOStore

	// LedgerSink is an optional reference to the client-side
	// ledger accounting actor. When set, the OOR actor forwards
	// VTXOSentMsg / VTXOReceivedMsg events as off-band-transfer
	// activity is finalized so the local accounting DB stays in
	// sync. When None, ledger emission is silently skipped --
	// useful for unit tests that do not register a ledger actor.
	LedgerSink fn.Option[ledger.Sink]
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
		ResolveIncomingTransferTLVType,
		func() actor.TLVMessage {
			return &ResolveIncomingTransferRequest{}
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
	if cfg.ActorID == "" {
		cfg.ActorID = fmt.Sprintf("oor-client-%s", uuid.NewString())
	}

	ctorLogger := cfg.Log.UnwrapOr(btclog.Disabled)
	ctorLogger.InfoS(context.Background(), "Creating OOR client actor",
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
		sessions: make(map[SessionID]*sessionHandle),
	}

	durableCfg := actor.DefaultDurableActorConfig[OORDurableMsg,
		ActorResp](
		cfg.ActorID,
		behavior,
		cfg.DeliveryStore,
		codec,
	)
	durableCfg.Log = cfg.Log

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

	ctorLogger.InfoS(context.Background(), "OOR durable actor started",
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

		ctorLogger.InfoS(
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

	ctx = build.ContextWithLogger(
		ctx, a.cfg.Log.UnwrapOr(build.LoggerFromContext(ctx)),
	)

	fut := a.ref.Ask(ctx, msg)

	return fut.Await(ctx)
}

// Stop shuts down the underlying durable actor and releases its goroutines.
//
// Stop is safe to call multiple times.
func (a *OORClientActor) Stop() {
	a.cfg.Log.UnwrapOr(btclog.Disabled).InfoS(
		context.Background(), "Stopping OOR client actor",
		slog.String("actor_id", a.cfg.ActorID),
	)

	if a.durable != nil {
		a.durable.Stop()
	}

	a.cfg.Log.UnwrapOr(btclog.Disabled).InfoS(
		context.Background(), "OOR client actor stopped",
		slog.String("actor_id", a.cfg.ActorID),
	)
}

// StopAndWait shuts down the underlying durable actor and waits for exit.
//
// StopAndWait is safe to call multiple times.
func (a *OORClientActor) StopAndWait(ctx context.Context) error {
	a.cfg.Log.UnwrapOr(build.LoggerFromContext(context.Background())).InfoS(
		context.Background(), "Stopping OOR client actor",
		slog.String("actor_id", a.cfg.ActorID),
	)

	if a.durable != nil {
		if err := a.durable.StopAndWait(ctx); err != nil {
			return err
		}
	}

	a.cfg.Log.UnwrapOr(build.LoggerFromContext(context.Background())).InfoS(
		context.Background(), "OOR client actor stopped",
		slog.String("actor_id", a.cfg.ActorID),
	)

	return nil
}

// oorDurableBehavior implements the durable actor behavior for the OOR
// client. It dispatches decoded TLV messages to per-session FSMs and
// persists a combined checkpoint after every state mutation.
type oorDurableBehavior struct {
	cfg ClientActorCfg

	sessions map[SessionID]*sessionHandle
}

// logger returns the configured logger or falls back to extracting from
// context. If no logger is found in either location, returns btclog.Disabled.
func (b *oorDurableBehavior) logger(ctx context.Context) btclog.Logger {
	return b.cfg.Log.UnwrapOr(build.LoggerFromContext(ctx))
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

	case *ResolveIncomingTransferRequest:
		return b.handleResolveIncomingTransfer(ctx, m)

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

	b.logger(ctx).InfoS(ctx, "Handling restart message",
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

	b.logger(ctx).InfoS(ctx, "Restart complete",
		slog.Int("num_sessions", len(b.sessions)))

	return fn.Ok[ActorResp](&DriveEventResponse{})
}

// handleStartTransfer starts a new outgoing transfer session.
func (b *oorDurableBehavior) handleStartTransfer(ctx context.Context,
	req *StartTransferRequest) fn.Result[ActorResp] {

	if req == nil {
		return fn.Err[ActorResp](fmt.Errorf("request must be provided"))
	}

	b.logger(ctx).InfoS(ctx, "Starting new OOR transfer",
		slog.Int("num_inputs", len(req.Inputs)),
		slog.Int("num_recipients", len(req.Recipients)))

	// Build the deterministic submit package and start the session FSM.
	// I/O is emitted as outbox messages.
	session, outbox, err := NewSessionWithIdempotencyKey(
		ctx, req.Policy, req.Inputs, req.Recipients, req.IdempotencyKey,
	)
	if err != nil {
		return fn.Err[ActorResp](err)
	}

	// StartTransferRequest is treated as idempotent: if the same
	// deterministic transfer is submitted twice (e.g. due to retries or
	// durable replay), we keep the existing session and return its ID.
	if _, exists := b.sessions[session.ID]; exists {
		//nolint:ll
		b.logger(ctx).DebugS(ctx, "Duplicate start transfer, returning existing session",
			slog.String("session_id", session.ID.String()))

		return fn.Ok[ActorResp](&StartTransferResponse{
			SessionID: session.ID,
		})
	}

	handle := &sessionHandle{
		FSM:            session.FSM,
		kind:           sessionKindOutgoing,
		IdempotencyKey: req.IdempotencyKey,
	}
	b.sessions[session.ID] = handle

	b.logger(ctx).InfoS(ctx, "OOR session created",
		slog.String("session_id", session.ID.String()),
		slog.Int("num_outbox", len(outbox)))

	err = b.persistCheckpoint(ctx)
	if err != nil {
		return fn.Err[ActorResp](err)
	}

	err = b.driveOutbox(ctx, session.ID, handle, outbox)
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

	b.logger(ctx).DebugS(ctx, "Driving event into session",
		slog.String("session_id", req.SessionID.String()),
		slog.String("event_type", fmt.Sprintf("%T", req.Event)))

	handle, ok := b.sessions[req.SessionID]
	if !ok {
		incoming, isIncoming := req.Event.(*IncomingTransferEvent)
		if !isIncoming {
			return fn.Err[ActorResp](fmt.Errorf(
				"unknown session: %s", req.SessionID,
			))
		}

		err := b.handleIncomingTransfer(ctx, req.SessionID, incoming)
		if err != nil {
			return fn.Err[ActorResp](err)
		}

		return fn.Ok[ActorResp](&DriveEventResponse{})
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
	handle.clearRetryMetadata()

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

	// Emit the outgoing-transfer ledger entry only after the
	// checkpoint commits: the caller contract for the ledger
	// actor is that we have a durable local record of the send
	// before posting to accounting, so a crash before checkpoint
	// persistence does not double-book the transfers_out leg on
	// replay.
	b.emitVTXOSent(ctx, req.SessionID, finalizeState)

	b.notifyMaterializedVTXOs(ctx, req.Event)

	err = b.driveOutbox(ctx, req.SessionID, handle, outbox)
	if err != nil {
		return fn.Err[ActorResp](err)
	}

	return fn.Ok[ActorResp](&DriveEventResponse{})
}

// handleResolveIncomingTransfer durably records a lightweight incoming OOR
// hint, then emits the transport query needed to resolve the full Ark package
// after the checkpoint commits.
func (b *oorDurableBehavior) handleResolveIncomingTransfer(
	ctx context.Context,
	req *ResolveIncomingTransferRequest) fn.Result[ActorResp] {

	if req == nil {
		return fn.Err[ActorResp](fmt.Errorf("request must be provided"))
	}

	if len(req.RecipientPkScript) == 0 {
		return fn.Err[ActorResp](fmt.Errorf(
			"recipient pk script must be provided",
		))
	}

	b.logger(ctx).DebugS(ctx, "Handling incoming transfer hint",
		slog.String("session_id", req.SessionID.String()),
		slog.Uint64("recipient_event_id", req.RecipientEventID),
		slog.String("recipient_pk_script",
			hex.EncodeToString(req.RecipientPkScript)))

	created := false
	handle, ok := b.sessions[req.SessionID]
	if ok && handle.kind != sessionKindIncoming {
		state, err := handle.currentSessionState()
		if err != nil {
			return fn.Err[ActorResp](err)
		}

		outgoingState, isOutgoingState := state.(State)
		if !isOutgoingState {
			return fn.Err[ActorResp](fmt.Errorf("session %s has "+
				"unexpected outgoing state type %T",
				req.SessionID, state))
		}

		if !outgoingState.IsTerminal() {
			b.logger(ctx).DebugS(
				ctx,
				"Deferring incoming self-transfer hint "+
					"until outgoing session reaches "+
					"terminal state",
				slog.String("session_id", req.SessionID.String()),
				slog.String("state", fmt.Sprintf("%T", state)),
			)

			return fn.Err[ActorResp](fmt.Errorf(
				"outgoing session %s still active "+
					"for incoming hint",
				req.SessionID,
			))
		}

		b.logger(ctx).DebugS(
			ctx, "Replacing terminal outgoing session "+
				"with incoming self-transfer session",
			slog.String("session_id", req.SessionID.String()),
			slog.String("state", fmt.Sprintf("%T", state)),
		)

		delete(b.sessions, req.SessionID)
		handle = nil
		ok = false
	}

	if !ok {
		session, err := newReceiveSessionWithState(
			ctx, req.SessionID, &ReceiveResolving{
				SessionID: req.SessionID,
				RecipientPkScript: append(
					[]byte(nil), req.RecipientPkScript...,
				),
				RecipientEventID: req.RecipientEventID,
			},
		)
		if err != nil {
			return fn.Err[ActorResp](err)
		}

		handle = &sessionHandle{
			FSM:  session.FSM,
			kind: sessionKindIncoming,
		}
		b.sessions[req.SessionID] = handle
		created = true

		err = b.persistCheckpoint(ctx)
		if err != nil {
			return fn.Err[ActorResp](err)
		}
	}

	if handle.kind != sessionKindIncoming {
		return fn.Err[ActorResp](fmt.Errorf("session %s is not "+
			"incoming", req.SessionID))
	}

	state, err := handle.currentSessionState()
	if err != nil {
		return fn.Err[ActorResp](err)
	}

	if _, ok := state.(*ReceiveResolving); !ok {
		b.logger(ctx).DebugS(ctx, "Ignoring duplicate incoming "+
			"transfer hint for active session",
			slog.String("session_id", req.SessionID.String()),
			slog.String("state", fmt.Sprintf("%T", state)))

		return fn.Ok[ActorResp](&DriveEventResponse{})
	}

	if !created {
		b.logger(ctx).DebugS(ctx, "Ignoring duplicate incoming "+
			"resolve request for pending session",
			slog.String("session_id", req.SessionID.String()))

		return fn.Ok[ActorResp](&DriveEventResponse{})
	}

	outbox, err := outboxForHandle(handle, state)
	if err != nil {
		return fn.Err[ActorResp](err)
	}

	err = b.driveOutbox(ctx, req.SessionID, handle, outbox)
	if err != nil {
		return fn.Err[ActorResp](err)
	}

	return fn.Ok[ActorResp](&DriveEventResponse{})
}

// handleIncomingTransfer drives a new incoming-transfer notification without
// requiring a pre-existing outgoing session entry.
//
// Incoming notifications originate at the transport boundary, so the actor
// must be able to materialize them the first time it sees the session ID.
func (b *oorDurableBehavior) handleIncomingTransfer(ctx context.Context,
	sessionID SessionID, event *IncomingTransferEvent) error {

	if event == nil {
		return fmt.Errorf("incoming event must be provided")
	}

	// Reject if a session with this ID already exists in any
	// kind (outgoing or incoming). A malicious server could
	// push an IncomingTransferEvent with a known outgoing txid
	// to create a shadow session that blocks outgoing restore.
	if _, exists := b.sessions[sessionID]; exists {
		return fmt.Errorf("session %s already exists, "+
			"rejecting incoming transfer", sessionID)
	}

	if event.SessionID != (SessionID{}) && event.SessionID != sessionID {
		return fmt.Errorf("incoming event session id mismatch")
	}

	b.logger(ctx).DebugS(ctx, "Handling incoming transfer event",
		slog.String("session_id", sessionID.String()),
		slog.Int("num_checkpoints", len(event.FinalCheckpointPSBTs)))

	session, outbox, err := DriveIncomingTransferWithCheckpoints(
		ctx, sessionID, event.ArkPSBT, event.FinalCheckpointPSBTs,
	)
	if err != nil {
		return err
	}

	b.logger(ctx).DebugS(ctx, "Incoming transfer produced outbox",
		slog.String("session_id", session.ID.String()),
		slog.Int("outbox_len", len(outbox)))

	handle := &sessionHandle{
		FSM:  session.FSM,
		kind: sessionKindIncoming,
	}
	b.sessions[session.ID] = handle

	err = b.persistCheckpoint(ctx)
	if err != nil {
		return err
	}

	return b.driveOutbox(ctx, session.ID, handle, outbox)
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

	state, err := handle.currentSessionState()
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

	b.logger(ctx).DebugS(ctx, "Persisting outgoing package",
		slog.String("session_id", sessionID.String()),
		slog.Int("num_inputs", len(state.TransferInputs)),
		slog.Int("num_checkpoints", len(state.FinalCheckpointPSBTs)))

	err := b.cfg.PackageStore.UpsertPackage(ctx,
		PackageDirectionOutgoing, sessionHash, state.ArkPSBT,
		state.FinalCheckpointPSBTs,
	)
	if err != nil {
		return err
	}

	outpoints := InputOutpoints(state.TransferInputs)
	for i := range outpoints {
		err := b.cfg.PackageStore.UpsertBinding(ctx,
			outpoints[i], sessionHash, uint32(i),
			PackageLinkKindConsumedInput,
		)
		if err != nil {
			isMissingBinding := errors.Is(
				err, libtypes.ErrOORBindingOutpointNotFound,
			)
			if isMissingBinding {
				b.logger(ctx).DebugS(ctx,
					"Skipping non-local outgoing package "+
						"input binding",
					slog.String("session_id", sessionID.String()),
					slog.String("outpoint",
						outpoints[i].String()),
				)

				continue
			}

			return err
		}
	}

	return nil
}

// emitVTXOSent posts a VTXOSentMsg to the ledger actor after a
// finalize event commits a v0 outgoing OOR transfer. AmountSat is
// the gross satoshi value consumed across all TransferInputs; OOR
// transfers are fee-less per the package invariant, so the same
// number equals the sum of recipient output values. Emission is
// best-effort: a failure is logged but does not fail the caller
// (accounting is a side observation, not a blocking pre-condition
// for the send having happened).
func (b *oorDurableBehavior) emitVTXOSent(ctx context.Context,
	sessionID SessionID, state *AwaitingFinalizeAccepted) {

	b.cfg.LedgerSink.WhenSome(func(sink ledger.Sink) {
		if state == nil || len(state.TransferInputs) == 0 {
			return
		}

		var total int64
		for i := range state.TransferInputs {
			total += int64(state.TransferInputs[i].VTXO.Amount)
		}
		if total <= 0 {
			return
		}

		msg := &ledger.VTXOSentMsg{
			SessionID: sessionID,
			AmountSat: total,
		}

		if err := sink.Tell(ctx, msg); err != nil {
			b.logger(ctx).WarnS(ctx,
				"Failed to emit VTXOSentMsg to ledger", err,
				slog.String("session_id", sessionID.String()),
				slog.Int64("amount_sat", total))
		}
	})
}

// emitVTXOsReceived posts a VTXOReceivedMsg per materialized
// incoming VTXO to the ledger actor. Incoming OOR transfers are
// already net of counterparty fees on the wire, so AmountSat is
// the descriptor Amount verbatim. Emission is best-effort: a
// per-VTXO failure is logged and the loop continues so the
// remaining VTXOs still get booked.
func (b *oorDurableBehavior) emitVTXOsReceived(ctx context.Context,
	descs []*vtxo.Descriptor) {

	b.cfg.LedgerSink.WhenSome(func(sink ledger.Sink) {
		for _, desc := range descs {
			if desc == nil {
				continue
			}

			msg := &ledger.VTXOReceivedMsg{
				OutpointHash:  desc.Outpoint.Hash,
				OutpointIndex: desc.Outpoint.Index,
				AmountSat:     int64(desc.Amount),
				Source:        ledger.SourceOOR,
			}

			if err := sink.Tell(ctx, msg); err != nil {
				b.logger(ctx).WarnS(ctx,
					"Failed to emit VTXOReceivedMsg to "+
						"ledger", err,
					slog.String("outpoint",
						desc.Outpoint.String()),
					slog.Int64("amount_sat",
						int64(desc.Amount)))
			}
		}
	})
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

	b.logger(ctx).InfoS(ctx, "Restoring session from snapshot",
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

	b.sessions[session.ID] = &sessionHandle{
		FSM:            session.FSM,
		kind:           sessionKindOutgoing,
		RetryAfter:     req.Snapshot.RetryAfter,
		RetryReason:    req.Snapshot.FailReason,
		IdempotencyKey: req.Snapshot.IdempotencyKey,
	}

	err = b.persistCheckpoint(ctx)
	if err != nil {
		return fn.Err[ActorResp](err)
	}

	b.logger(ctx).InfoS(ctx, "Session restored successfully",
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

	b.logger(ctx).InfoS(ctx, "Resuming session",
		slog.String("session_id", req.SessionID.String()))

	handle, ok := b.sessions[req.SessionID]
	if !ok {
		return fn.Err[ActorResp](fmt.Errorf("unknown session: %s",
			req.SessionID))
	}

	state, err := handle.currentSessionState()
	if err != nil {
		return fn.Err[ActorResp](err)
	}

	b.logger(ctx).DebugS(ctx, "Session current state for resume",
		slog.String("session_id", req.SessionID.String()),
		slog.String("state", fmt.Sprintf("%T", state)))

	outbox, err := outboxForHandle(handle, state)
	if err != nil {
		return fn.Err[ActorResp](err)
	}
	handle.clearRetryMetadata()

	err = b.persistCheckpoint(ctx)
	if err != nil {
		return fn.Err[ActorResp](err)
	}

	err = b.driveOutbox(ctx, req.SessionID, handle, outbox)
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

	if handle.kind != sessionKindOutgoing {
		return fn.Err[ActorResp](fmt.Errorf("export snapshot only " +
			"supports outgoing sessions"))
	}

	state, err := handle.currentOutgoingState()
	if err != nil {
		return fn.Err[ActorResp](err)
	}

	snapshot, err := NewOutgoingSnapshot(req.SessionID, state)
	if err != nil {
		return fn.Err[ActorResp](err)
	}
	handle.applyRetrySnapshot(snapshot)

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

	state, err := handle.currentSessionState()
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

	if len(raw) == 0 {
		return nil
	}

	checkpoint, err := decodeSessionsCheckpoint(raw)
	if err != nil {
		return err
	}

	if checkpoint.Version < 1 || checkpoint.Version > oorCheckpointVersion {
		return fmt.Errorf("unknown checkpoint version: %d",
			checkpoint.Version)
	}

	b.logger(ctx).InfoS(ctx, "Restoring sessions from checkpoint",
		slog.Int("checkpoint_version", checkpoint.Version),
		slog.Int("num_outgoing_snapshots",
			len(checkpoint.OutgoingSnapshots)),
		slog.Int("num_incoming_snapshots",
			len(checkpoint.IncomingSnapshots)))

	for i := range checkpoint.OutgoingSnapshots {
		snapshot := checkpoint.OutgoingSnapshots[i]

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

		b.sessions[session.ID] = &sessionHandle{
			FSM:            session.FSM,
			kind:           sessionKindOutgoing,
			RetryAfter:     snapshot.RetryAfter,
			RetryReason:    snapshot.FailReason,
			IdempotencyKey: snapshot.IdempotencyKey,
		}

		b.logger(ctx).DebugS(ctx, "Restored session from checkpoint",
			slog.String("session_id", session.ID.String()))
	}

	for i := range checkpoint.IncomingSnapshots {
		snapshot := checkpoint.IncomingSnapshots[i]

		if _, exists := b.sessions[snapshot.SessionID]; exists {
			return fmt.Errorf(
				"duplicate session id in checkpoint: %s",
				snapshot.SessionID,
			)
		}

		session, err := NewReceiveSessionFromSnapshot(ctx, snapshot)
		if err != nil {
			return err
		}

		b.sessions[session.ID] = &sessionHandle{
			FSM:  session.FSM,
			kind: sessionKindIncoming,
		}

		b.logger(ctx).DebugS(ctx, "Restored incoming session "+
			"from checkpoint",
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

	b.logger(ctx).InfoS(ctx, "Resuming restored sessions",
		slog.Int("num_sessions", len(sessionIDs)))

	for i := range sessionIDs {
		sessionID := sessionIDs[i]
		handle := b.sessions[sessionID]

		state, err := handle.currentSessionState()
		if err != nil {
			return err
		}

		outbox, err := b.resumeOutboxForHandle(handle, state)
		if err != nil {
			return err
		}

		b.logger(ctx).DebugS(ctx, "Resuming restored session",
			slog.String("session_id", sessionID.String()),
			slog.String("state", fmt.Sprintf("%T", state)),
			slog.Int("num_outbox", len(outbox)))

		err = b.driveOutbox(ctx, sessionID, handle, outbox)
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

// NOTE: MarkInputsSpentRequest and ScheduleRetryRequest have ToProto methods
// for TLV persistence but intentionally do NOT implement
// serverconn.ServerMessage (they lack ServiceMethod). Routing them to the
// server would cause fund-loss (inputs not marked spent locally) or liveness
// failure (retry timers lost). The isTransportEvent type switch below
// enumerates only the true transport types.

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
		*SendIncomingAckRequest, *QueryIncomingTransferRequest,
		*QueryIncomingMetadataRequest:

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
	switch queryReq := msg.(type) {
	case *QueryIncomingTransferRequest:
		afterEventID := uint64(0)
		if queryReq.RecipientEventID > 0 {
			afterEventID = queryReq.RecipientEventID - 1
		}

		//nolint:ll
		sendReq := &serverconn.SendListOORRecipientEventsByScriptRequest{
			PkScript: append(
				[]byte(nil), queryReq.RecipientPkScript...,
			),
			AfterEventID: afterEventID,
			Limit:        1,
			CorrelationID: IncomingResolveCorrelationID(
				queryReq.SessionID, queryReq.RecipientEventID,
			),
		}

		if err := b.cfg.ServerConn.Tell(ctx, sendReq); err != nil {
			return fmt.Errorf("send incoming resolve query to "+
				"server: %w", err)
		}

		return nil

	case *QueryIncomingMetadataRequest:
		recipients := queryReq.Recipients

		filter, ok := b.cfg.OutboxHandler.(incomingMetadataFilter)
		if ok {
			var err error
			owned, err := filter.FilterIncomingMetadataRecipients(
				ctx, queryReq.Recipients,
			)
			if err != nil {
				return fmt.Errorf("filter incoming metadata "+
					"recipients: %w", err)
			}

			recipients = owned
		}

		if len(recipients) == 0 {
			return fmt.Errorf("incoming metadata query " +
				"contains no wallet-owned recipients")
		}

		pkScripts := make([][]byte, 0, len(recipients))
		for i := range recipients {
			pkScripts = append(pkScripts, append(
				[]byte(nil), recipients[i].PkScript...,
			))
		}

		sendReq := &serverconn.SendListVTXOsByScriptsRequest{
			PkScripts: pkScripts,
			Limit:     incomingMetadataQueryLimit,
			CorrelationID: IncomingMetadataCorrelationID(
				queryReq.SessionID,
			),
		}

		if err := b.cfg.ServerConn.Tell(ctx, sendReq); err != nil {
			return fmt.Errorf("send metadata query to server: %w",
				err)
		}

		return nil
	}

	if !ok {
		return fmt.Errorf("transport event %T does not implement "+
			"ServerMessage", msg)
	}

	sm := serverMsg.ServiceMethod()
	sendReq := &serverconn.SendClientEventRequest{
		Message: serverMsg,
		Service: sm.Service,
		Method:  sm.Method,
	}

	if err := b.cfg.ServerConn.Tell(ctx, sendReq); err != nil {
		return fmt.Errorf("send transport event to server: %w", err)
	}

	return nil
}

// driveOutbox executes outbox work using the configured handler and feeds any
// follow-up events back into the FSM.
func (b *oorDurableBehavior) driveOutbox(ctx context.Context,
	sessionID SessionID, handle *sessionHandle,
	outbox []OutboxEvent) error {

	handler := b.cfg.OutboxHandler

	if handle == nil {
		return fmt.Errorf("session handle must be provided")
	}

	for _, msg := range outbox {
		// Transport events (submit, finalize, ack) are Tell'd to
		// the serverconn actor for durable delivery. The FSM stays
		// in its AwaitingX state until the server response arrives
		// asynchronously via DriveEventRequest.
		if b.isTransportEvent(msg) {
			//nolint:ll
			b.logger(ctx).DebugS(ctx, "Sending transport event to server",
				slog.String("session_id", sessionID.String()),
				slog.String("event_type", fmt.Sprintf("%T", msg)))

			if err := b.sendTransportEvent(ctx, msg); err != nil {
				return err
			}

			if _, ok := msg.(*SendIncomingAckRequest); ok {
				nextOutbox, err := b.askEvent(
					ctx,
					handle.FSM,
					&IncomingAckSentEvent{},
				)
				if err != nil {
					return err
				}

				err = b.persistCheckpoint(ctx)
				if err != nil {
					return err
				}

				err = b.driveOutbox(
					ctx, sessionID, handle, nextOutbox,
				)
				if err != nil {
					return err
				}
			}

			continue
		}

		b.logger(ctx).DebugS(ctx, "Handling local outbox event",
			slog.String("session_id", sessionID.String()),
			slog.String("event_type", fmt.Sprintf("%T", msg)))

		if handler == nil {
			return fmt.Errorf("outbox handler must " +
				"be provided for local events")
		}

		// Local events (signing, persistence, timers) continue
		// through the outbox handler.
		followUps, err := handler.Handle(ctx, sessionID, msg)
		if err != nil {
			//nolint:ll
			b.logger(ctx).WarnS(ctx, "Outbox handler error, wrapping as retryable event", err,
				slog.String("session_id", sessionID.String()),
				slog.String("event_type", fmt.Sprintf("%T", msg)))

			followUps = []Event{
				NewOutboxErrorEvent(msg, err),
			}
		}

		for _, followUp := range followUps {
			// When incoming VTXOs are materialized, forward
			// them to the VTXO manager so it can spawn
			// monitoring actors. This mirrors the rounds
			// actor pattern for VTXOCreatedNotification.
			b.notifyMaterializedVTXOs(ctx, followUp)

			finalizeState, err := b.captureFinalizeStateForEvent(
				handle.FSM, followUp,
			)
			if err != nil {
				return err
			}

			// Feed follow-up events into the FSM.
			// Recursively execute any emitted outbox work.
			// Stop when none remains.
			nextOutbox, err := b.askEvent(ctx, handle.FSM, followUp)
			if err != nil {
				return err
			}
			handle.applyRetryEvent(followUp)

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

			err = b.driveOutbox(ctx, sessionID, handle, nextOutbox)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

// notifyMaterializedVTXOs forwards newly materialized incoming VTXOs to the
// VTXO manager when the follow-up event carries descriptors. This mirrors
// the rounds actor pattern where VTXOCreatedNotification is Tell'd to the
// manager from the actor's dispatch loop.
func (b *oorDurableBehavior) notifyMaterializedVTXOs(ctx context.Context,
	followUp Event) {

	handled, ok := followUp.(*IncomingHandledEvent)
	if !ok {
		return
	}

	if b.cfg.VTXOManager == nil {
		return
	}

	descs := handled.MaterializedVTXOs
	if len(descs) == 0 {
		descs = b.loadMaterializedVTXOs(ctx, handled)
	}

	if len(descs) == 0 {
		return
	}

	notification := &vtxo.VTXOsMaterializedNotification{
		VTXOs: descs,
	}

	if err := b.cfg.VTXOManager.Tell(ctx, notification); err != nil {
		b.logger(ctx).WarnS(
			ctx, "Failed to notify VTXO manager of "+
				"materialized incoming VTXOs", err,
			slog.Int("num_vtxos", len(descs)))
	}

	b.emitVTXOsReceived(ctx, descs)
}

// loadMaterializedVTXOs reloads persisted incoming VTXO descriptors for a
// callback event that only round-tripped outpoint identifiers through the
// durable mailbox.
func (b *oorDurableBehavior) loadMaterializedVTXOs(ctx context.Context,
	handled *IncomingHandledEvent) []*vtxo.Descriptor {

	if handled == nil || len(handled.MaterializedOutpoints) == 0 {
		return nil
	}

	if b.cfg.VTXOStore == nil {
		b.logger(ctx).WarnS(
			ctx, "Missing VTXO store for incoming callback reload",
			nil,
			slog.Int("num_outpoints",
				len(handled.MaterializedOutpoints)))

		return nil
	}

	descs := make([]*vtxo.Descriptor, 0,
		len(handled.MaterializedOutpoints))

	for _, outpoint := range handled.MaterializedOutpoints {
		desc, err := b.cfg.VTXOStore.GetVTXO(ctx, outpoint)
		if err != nil {
			b.logger(ctx).WarnS(
				ctx, "Failed to reload materialized incoming "+
					"VTXO for manager notification", err,
				slog.String("outpoint", outpoint.String()))

			continue
		}

		descs = append(descs, desc)
	}

	return descs
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

	outgoingSnapshots := make(
		[]*OutgoingSnapshot, 0, len(sessionIDs),
	)
	incomingSnapshots := make(
		[]*IncomingSnapshot, 0, len(sessionIDs),
	)
	for i := range sessionIDs {
		sessionID := sessionIDs[i]
		handle := b.sessions[sessionID]

		state, err := handle.currentSessionState()
		if err != nil {
			return err
		}

		switch handle.kind {
		case sessionKindOutgoing:
			outgoingState, ok := state.(State)
			if !ok {
				return fmt.Errorf("unexpected outgoing state "+
					"type: %T", state)
			}

			snapshot, err := NewOutgoingSnapshot(
				sessionID, outgoingState,
			)
			if err != nil {
				return err
			}
			handle.applyRetrySnapshot(snapshot)

			outgoingSnapshots = append(
				outgoingSnapshots, snapshot,
			)

		case sessionKindIncoming:
			snapshot, err := NewIncomingSnapshot(
				sessionID, state,
			)
			if err != nil {
				return err
			}

			incomingSnapshots = append(
				incomingSnapshots, snapshot,
			)

		default:
			return fmt.Errorf("unknown session kind: %d",
				handle.kind)
		}
	}

	raw, err := encodeSessionsCheckpoint(sessionsCheckpoint{
		Version:           oorCheckpointVersion,
		OutgoingSnapshots: outgoingSnapshots,
		IncomingSnapshots: incomingSnapshots,
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

	kind sessionKind

	RetryAfter  time.Duration
	RetryReason string

	IdempotencyKey string
}

type sessionKind uint8

const (
	sessionKindOutgoing sessionKind = iota + 1
	sessionKindIncoming
)

// currentSessionState returns the current concrete OOR session state.
func (h *sessionHandle) currentSessionState() (SessionState, error) {
	current, err := h.FSM.CurrentState()
	if err != nil {
		return nil, err
	}

	return current, nil
}

// currentOutgoingState returns the current outgoing session state.
func (h *sessionHandle) currentOutgoingState() (State, error) {
	state, err := h.currentSessionState()
	if err != nil {
		return nil, err
	}

	outgoingState, ok := state.(State)
	if !ok {
		return nil, fmt.Errorf(
			"unexpected outgoing state type: %T",
			state,
		)
	}

	return outgoingState, nil
}

// outboxForHandle returns the outbox implied by the handle's current state.
func outboxForHandle(handle *sessionHandle,
	state SessionState) ([]OutboxEvent, error) {

	if handle == nil {
		return nil, fmt.Errorf("session handle must be provided")
	}

	switch handle.kind {
	case sessionKindOutgoing:
		outgoingState, ok := state.(State)
		if !ok {
			return nil, fmt.Errorf(
				"unexpected outgoing state type: %T",
				state,
			)
		}

		return OutboxForState(outgoingState)

	case sessionKindIncoming:
		return OutboxForIncomingState(state)

	default:
		return nil, fmt.Errorf("unknown session kind: %d", handle.kind)
	}
}

// applyRetryEvent updates retry metadata based on a follow-up event result.
func (h *sessionHandle) applyRetryEvent(event Event) {
	if h == nil {
		return
	}

	retryEvent, ok := event.(*OutboxErrorEvent)
	if !ok || !retryEvent.Retryable {
		h.clearRetryMetadata()
		return
	}

	after := retryEvent.RetryAfter
	if after == 0 {
		after = defaultRetryDelay
	}

	h.RetryAfter = after
	h.RetryReason = retryEvent.ErrorReason
}

// clearRetryMetadata removes any pending retry scheduling metadata.
func (h *sessionHandle) clearRetryMetadata() {
	if h == nil {
		return
	}

	h.RetryAfter = 0
	h.RetryReason = ""
}

// applyRetrySnapshot copies retry metadata and the idempotency key onto an
// exported snapshot.
func (h *sessionHandle) applyRetrySnapshot(snapshot *OutgoingSnapshot) {
	if h == nil || snapshot == nil {
		return
	}

	if snapshot.IdempotencyKey == "" {
		snapshot.IdempotencyKey = h.IdempotencyKey
	}

	if h.RetryAfter == 0 {
		return
	}

	snapshot.RetryAfter = h.RetryAfter
	snapshot.FailReason = h.RetryReason
}

// resumeOutboxForHandle returns either retry scheduling or the state's
// natural outbox, depending on whether retry metadata is pending.
func (b *oorDurableBehavior) resumeOutboxForHandle(
	handle *sessionHandle, state SessionState,
) ([]OutboxEvent, error) {

	if handle == nil {
		return nil, fmt.Errorf("session handle must be provided")
	}

	switch handle.kind {
	case sessionKindOutgoing:
		outgoingState, ok := state.(State)
		if !ok {
			return nil, fmt.Errorf(
				"unexpected outgoing state type: %T",
				state,
			)
		}

		if handle.RetryAfter > 0 {
			return []OutboxEvent{
				&ScheduleRetryRequest{
					After:  handle.RetryAfter,
					Reason: handle.RetryReason,
				},
			}, nil
		}

		return OutboxForState(outgoingState)

	case sessionKindIncoming:
		return OutboxForIncomingState(state)

	default:
		return nil, fmt.Errorf("unknown session kind: %d",
			handle.kind)
	}
}

type durableBehaviorIface = actor.ActorBehavior[
	OORDurableMsg, ActorResp,
]

var _ durableBehaviorIface = (*oorDurableBehavior)(nil)
