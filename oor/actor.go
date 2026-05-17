//nolint:ll
package oor

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
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

// OutboxHandler executes FSM outbox requests and returns follow-up events.
//
// This mirrors the server-side OOR coordinator approach. The goal is to keep
// the FSM pure and move I/O (RPC, signing, persistence) behind an explicit
// boundary that can later be implemented by local actors.
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

	// Limits configures defense-in-depth bounds for incoming OOR
	// receive payloads. Zero fields use DefaultReceiveLimits.
	Limits ReceiveLimits

	// ServerConn is a reference to the ServerConnectionActor for sending
	// transport events (submit, finalize, ack) to the server. When set,
	// transport outbox events bypass the OutboxHandler and are Tell'd to
	// the connection actor for durable delivery. When nil, all outbox
	// events are routed through OutboxHandler for backward compatibility.
	ServerConn actor.TellOnlyRef[serverconn.ServerConnMsg]

	// PackageStore persists finalized outgoing packages and local input
	// bindings used by unroll/recovery tooling.
	PackageStore PackagePersistence

	// SessionStore persists OOR-owned session rows and artifacts.
	SessionStore OORClientSessionStore

	// UseSQLEffects makes the coordinator persist session state and implied
	// side effects, then lets OORClientEffectWorker execute those effects
	// out-of-band. When false, the coordinator executes outbox work inline.
	UseSQLEffects bool

	// ActorSystem is the system in which the OOR actor registers itself
	// under the OOR service key. This enables serverconn ingress
	// dispatching and timeout callback wiring via service key lookup.
	// When nil, the actor is not registered (useful for unit tests).
	ActorSystem actor.SystemContext

	// ActorID is the stable service id used for this actor instance.
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

	// IncomingVTXOObserver receives incoming VTXO descriptors after
	// materialization so daemon-local subsystems can arm actor-owned
	// work without making this package depend on those subsystems.
	IncomingVTXOObserver IncomingVTXONotifier
}

// OORClientActor wraps the outgoing-transfer client FSM in an in-memory actor
// interface.
//
// The actor owns a set of per-session protofsm state machines and drives them
// by executing outbox requests via an OutboxHandler.
type OORClientActor struct {
	cfg ClientActorCfg

	ref     actor.ActorRef[ActorMsg, ActorResp]
	runtime *actor.Actor[ActorMsg, ActorResp]
	wg      sync.WaitGroup

	startupErr error
}

// NewOORClientActor creates an outgoing-transfer OOR client actor.
//
// Startup restores SQL-backed workflow state before the in-memory actor starts
// processing messages. If startup prerequisites fail, the returned actor stores
// the error and surfaces it on Receive.
func NewOORClientActor(cfg ClientActorCfg) *OORClientActor {
	cfg.Limits = normalizeReceiveLimits(cfg.Limits)

	if cfg.ActorID == "" {
		cfg.ActorID = fmt.Sprintf("oor-client-%s", uuid.NewString())
	}

	ctorLogger := cfg.Log.UnwrapOr(btclog.Disabled)
	ctorLogger.InfoS(context.Background(), "Creating OOR client actor",
		slog.String("actor_id", cfg.ActorID),
	)

	actorRef := &OORClientActor{cfg: cfg}

	if cfg.SessionStore == nil {
		actorRef.startupErr = fmt.Errorf("session store must be " +
			"provided")

		return actorRef
	}

	behavior := &oorDurableBehavior{
		cfg:      cfg,
		sessions: make(map[SessionID]*sessionHandle),
		pendingIncoming: make(
			map[SessionID]*ResolveIncomingTransferRequest,
		),
	}

	restoreCtx := build.ContextWithLogger(
		context.Background(), ctorLogger,
	)
	restore := behavior.restoreSessionState(restoreCtx)
	if restore.IsErr() {
		actorRef.startupErr = restore.Err()

		return actorRef
	}

	runtime := actor.NewActor(actor.ActorConfig[
		ActorMsg, ActorResp,
	]{
		ID:          cfg.ActorID,
		Behavior:    behavior,
		MailboxSize: 128,
		Wg:          &actorRef.wg,
	})
	actorRef.runtime = runtime
	actorRef.ref = runtime.Ref()
	runtime.Start()

	ctorLogger.InfoS(context.Background(), "OOR client actor started",
		slog.String("actor_id", cfg.ActorID),
	)

	// Register the actor ref with the actor system so the
	// serverconn event router can discover it via the OOR service key.
	if cfg.ActorSystem != nil {
		oorKey := NewServiceKey()
		err := actor.RegisterWithReceptionist(
			cfg.ActorSystem.Receptionist(), oorKey, runtime.Ref(),
		)
		if err != nil {
			actorRef.startupErr = fmt.Errorf("register OOR "+
				"actor: %w", err)

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

// Receive sends an actor message through the in-memory mailbox and returns
// the response synchronously.
func (a *OORClientActor) Receive(ctx context.Context,
	msg ActorMsg) fn.Result[ActorResp] {

	if a.startupErr != nil {
		return fn.Err[ActorResp](a.startupErr)
	}

	if a.ref == nil {
		return fn.Err[ActorResp](
			fmt.Errorf("actor not initialized"),
		)
	}

	if msg == nil {
		return fn.Err[ActorResp](
			fmt.Errorf("message must be provided"),
		)
	}

	ctx = build.ContextWithLogger(
		ctx,
		a.cfg.Log.UnwrapOr(
			build.LoggerFromContext(ctx),
		),
	)

	fut := a.ref.Ask(ctx, msg)

	return fut.Await(ctx)
}

// Stop shuts down the underlying in-memory actor and releases its goroutines.
//
// Stop is safe to call multiple times.
func (a *OORClientActor) Stop() {
	a.cfg.Log.UnwrapOr(btclog.Disabled).InfoS(
		context.Background(),
		"Stopping OOR client actor",
		slog.String("actor_id", a.cfg.ActorID),
	)

	if a.runtime != nil {
		a.runtime.Stop()
	}

	a.cfg.Log.UnwrapOr(btclog.Disabled).InfoS(
		context.Background(),
		"OOR client actor stopped",
		slog.String("actor_id", a.cfg.ActorID),
	)
}

// StopAndWait shuts down the underlying in-memory actor and waits for exit.
//
// StopAndWait is safe to call multiple times.
func (a *OORClientActor) StopAndWait(ctx context.Context) error {
	a.cfg.Log.UnwrapOr(build.LoggerFromContext(ctx)).InfoS(
		ctx,
		"Stopping OOR client actor",
		slog.String("actor_id", a.cfg.ActorID),
	)

	if a.runtime != nil {
		a.runtime.Stop()
		done := make(chan struct{})
		go func() {
			defer close(done)
			a.wg.Wait()
		}()

		select {
		case <-ctx.Done():
			return ctx.Err()

		case <-done:
		}
	}

	a.cfg.Log.UnwrapOr(build.LoggerFromContext(ctx)).InfoS(
		ctx,
		"OOR client actor stopped",
		slog.String("actor_id", a.cfg.ActorID),
	)

	return nil
}

// oorDurableBehavior implements the in-memory OOR client behavior. It
// dispatches actor messages to per-session FSMs and persists domain session
// rows after state changes.
type oorDurableBehavior struct {
	cfg ClientActorCfg

	sessions        map[SessionID]*sessionHandle
	pendingIncoming map[SessionID]*ResolveIncomingTransferRequest
}

// logger returns the configured logger or falls back to extracting from
// context. If no logger is found in either location, returns btclog.Disabled.
func (b *oorDurableBehavior) logger(ctx context.Context) btclog.Logger {
	return b.cfg.Log.UnwrapOr(build.LoggerFromContext(ctx))
}

// Receive dispatches actor messages to the appropriate handler method.
func (b *oorDurableBehavior) Receive(ctx context.Context,
	msg ActorMsg) fn.Result[ActorResp] {

	switch m := msg.(type) {
	case *StartTransferRequest:
		return b.handleStartTransfer(ctx, m)

	case *FindOutgoingSessionByIdempotencyKeyRequest:
		return b.handleFindOutgoingSessionByIdempotencyKey(ctx, m)

	case *ListSessionsRequest:
		return b.handleListSessions(ctx, m)

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
		return fn.Err[ActorResp](
			fmt.Errorf("unknown message type: %T", m),
		)
	}
}

// handleListSessions returns compact summaries for in-memory OOR sessions.
func (b *oorDurableBehavior) handleListSessions(_ context.Context,
	req *ListSessionsRequest) fn.Result[ActorResp] {

	if req == nil {
		return fn.Err[ActorResp](fmt.Errorf("request must be provided"))
	}

	summaries := make([]SessionSummary, 0, len(b.sessions))
	for sessionID, handle := range b.sessions {
		summary, err := summarizeSession(sessionID, handle)
		if err != nil {
			return fn.Err[ActorResp](err)
		}

		if !summaryMatchesListSessionsRequest(summary, req) {
			continue
		}

		summaries = append(summaries, summary)
	}

	sort.Slice(summaries, func(i, j int) bool {
		return summaries[i].SessionID.String() <
			summaries[j].SessionID.String()
	})

	return fn.Ok[ActorResp](&ListSessionsResponse{
		Sessions: summaries,
	})
}

// summaryMatchesListSessionsRequest applies direction and pending filters.
func summaryMatchesListSessionsRequest(summary SessionSummary,
	req *ListSessionsRequest) bool {

	if req.PendingOnly && !summary.Pending {
		return false
	}

	switch req.Direction {
	case SessionDirectionAll:
		return true

	case SessionDirectionOutgoing, SessionDirectionIncoming:
		return summary.Direction == req.Direction

	default:
		return false
	}
}

// summarizeSession projects a session handle into a compact status summary.
func summarizeSession(sessionID SessionID,
	handle *sessionHandle) (SessionSummary, error) {

	if handle == nil {
		return SessionSummary{}, fmt.Errorf("session handle must be " +
			"provided")
	}

	state, err := handle.currentSessionState()
	if err != nil {
		return SessionSummary{}, err
	}

	summary := SessionSummary{
		SessionID:   sessionID,
		Pending:     !state.IsTerminal(),
		RetryAfter:  handle.RetryAfter,
		RetryReason: handle.RetryReason,
	}

	switch handle.kind {
	case sessionKindOutgoing:
		summary.Direction = SessionDirectionOutgoing
		err := fillOutgoingSessionSummary(&summary, sessionID, state)
		if err != nil {
			return SessionSummary{}, err
		}

	case sessionKindIncoming:
		summary.Direction = SessionDirectionIncoming
		err := fillIncomingSessionSummary(&summary, sessionID, state)
		if err != nil {
			return SessionSummary{}, err
		}

	default:
		return SessionSummary{}, fmt.Errorf("unknown session kind: %d",
			handle.kind)
	}

	if failed, ok := state.(*Failed); ok && summary.RetryReason == "" {
		summary.RetryReason = failed.Reason
	}

	return summary, nil
}

// fillOutgoingSessionSummary populates outgoing-specific summary fields.
func fillOutgoingSessionSummary(summary *SessionSummary, sessionID SessionID,
	state SessionState) error {

	outgoingState, ok := state.(State)
	if !ok {
		return fmt.Errorf("unexpected outgoing state type: %T", state)
	}

	snapshot, err := NewOutgoingSnapshot(sessionID, outgoingState)
	if err != nil {
		return err
	}

	summary.Phase = string(snapshot.Phase)

	for i := range snapshot.TransferInputSnapshots {
		input := snapshot.TransferInputSnapshots[i]
		if input == nil {
			continue
		}

		summary.InputOutpoints = append(
			summary.InputOutpoints, input.Outpoint,
		)
		summary.InputAmountSat += input.AmountSat
	}

	summary.RecipientCount = outgoingRecipientCount(outgoingState)

	return nil
}

// fillIncomingSessionSummary populates incoming-specific summary fields.
func fillIncomingSessionSummary(summary *SessionSummary, sessionID SessionID,
	state SessionState) error {

	snapshot, err := NewIncomingSnapshot(sessionID, state)
	if err != nil {
		return err
	}

	summary.Phase = string(snapshot.Phase)

	return nil
}

// outgoingRecipientCount returns known recipient cardinality for live states.
func outgoingRecipientCount(state State) int {
	switch s := state.(type) {
	case *AwaitingArkSignatures:
		return len(s.RecipientOutputs)

	case *AwaitingSubmitAccepted:
		return len(s.RecipientOutputs)

	default:
		return 0
	}
}

// handleFindOutgoingSessionByIdempotencyKey returns an already-known outgoing
// session before callers acquire new inputs for a keyed retry.
func (b *oorDurableBehavior) handleFindOutgoingSessionByIdempotencyKey(
	ctx context.Context,
	req *FindOutgoingSessionByIdempotencyKeyRequest) fn.Result[ActorResp] {

	if req == nil {
		return fn.Err[ActorResp](fmt.Errorf("request must be provided"))
	}

	sessionID, found, err := b.findOutgoingSessionByIdempotencyKey(
		ctx, req.IdempotencyKey,
	)
	if err != nil {
		return fn.Err[ActorResp](err)
	}
	if !found {
		return fn.Ok[ActorResp](
			&FindOutgoingSessionByIdempotencyKeyResponse{},
		)
	}

	b.logger(ctx).DebugS(ctx, "Found existing OOR transfer",
		slog.String("session_id", sessionID.String()),
		slog.String("idempotency_key", req.IdempotencyKey),
	)

	return fn.Ok[ActorResp](&FindOutgoingSessionByIdempotencyKeyResponse{
		SessionID: sessionID,
		Found:     true,
	})
}

func (b *oorDurableBehavior) restoreSessionState(
	ctx context.Context) fn.Result[ActorResp] {

	b.sessions = make(map[SessionID]*sessionHandle)
	b.pendingIncoming = make(map[SessionID]*ResolveIncomingTransferRequest)

	if b.cfg.SessionStore == nil {
		return fn.Err[ActorResp](
			fmt.Errorf("session store must be provided"),
		)
	}

	if err := b.restoreFromSessionStore(ctx); err != nil {
		return fn.Err[ActorResp](err)
	}

	if err := b.resumeRestoredSessions(ctx); err != nil {
		return fn.Err[ActorResp](err)
	}

	if err := b.persistSessionState(ctx); err != nil {
		return fn.Err[ActorResp](err)
	}

	b.logger(ctx).InfoS(ctx, "SQL session restore complete",
		slog.Int("num_sessions", len(b.sessions)),
	)

	return fn.Ok[ActorResp](&DriveEventResponse{})
}

func (b *oorDurableBehavior) restoreFromSessionStore(
	ctx context.Context) error {

	sessions, err := b.cfg.SessionStore.LoadActiveSessions(ctx)
	if err != nil {
		return err
	}

	b.logger(ctx).InfoS(ctx, "Restoring sessions from SQL store",
		slog.Int("num_sessions", len(sessions)),
	)

	for i := range sessions {
		stored := sessions[i]

		switch stored.Direction {
		case SessionDirectionOutgoing:
			if stored.Outgoing == nil {
				return fmt.Errorf("stored outgoing session " +
					"missing snapshot")
			}

			session, err := NewSessionFromSnapshot(
				ctx, stored.Outgoing,
			)
			if err != nil {
				return err
			}

			if _, exists := b.sessions[session.ID]; exists {
				return fmt.Errorf("duplicate stored "+
					"session id: %s", session.ID)
			}

			b.sessions[session.ID] = &sessionHandle{
				FSM:            session.FSM,
				kind:           sessionKindOutgoing,
				RetryAfter:     stored.Outgoing.RetryAfter,
				RetryReason:    stored.Outgoing.FailReason,
				IdempotencyKey: stored.Outgoing.IdempotencyKey,
			}

		case SessionDirectionIncoming:
			if stored.Incoming == nil {
				return fmt.Errorf("stored incoming session " +
					"missing snapshot")
			}

			if existing, exists := b.sessions[stored.
				Incoming.
				SessionID]; exists {

				if existing.kind == sessionKindOutgoing &&
					stored.Incoming.Phase ==
						IncomingPhaseResolvePending {

					b.stagePendingIncoming(
						resolveRequestFromIncomingSnapshot(
							stored.Incoming,
						),
					)

					continue
				}

				return fmt.Errorf("duplicate stored "+
					"session id: %s",
					stored.Incoming.SessionID)
			}

			session, err := NewReceiveSessionFromSnapshot(
				ctx, stored.Incoming,
			)
			if err != nil {
				return err
			}

			b.sessions[session.ID] = &sessionHandle{
				FSM:  session.FSM,
				kind: sessionKindIncoming,
			}

		default:
			return fmt.Errorf("unknown stored session "+
				"direction: %d", stored.Direction)
		}
	}

	return nil
}

// handleStartTransfer starts a new outgoing transfer session.
func (b *oorDurableBehavior) handleStartTransfer(ctx context.Context,
	req *StartTransferRequest) fn.Result[ActorResp] {

	if req == nil {
		return fn.Err[ActorResp](fmt.Errorf("request must be provided"))
	}

	b.logger(ctx).InfoS(ctx, "Starting new OOR transfer",
		slog.Int("num_inputs", len(req.Inputs)),
		slog.Int("num_recipients", len(req.Recipients)),
	)

	existingSessionID, found, err := b.findOutgoingSessionByIdempotencyKey(
		ctx, req.IdempotencyKey,
	)
	if err != nil {
		return fn.Err[ActorResp](err)
	}
	if found {
		b.logger(ctx).InfoS(ctx, "Returning existing OOR transfer",
			slog.String("session_id", existingSessionID.String()),
			slog.String("idempotency_key", req.IdempotencyKey),
		)

		return fn.Ok[ActorResp](&StartTransferResponse{
			SessionID: existingSessionID,
			Existing:  true,
		})
	}

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
		b.logger(ctx).DebugS(
			ctx,
			"Duplicate start transfer, returning existing session",
			slog.String("session_id", session.ID.String()),
		)

		return fn.Ok[ActorResp](&StartTransferResponse{
			SessionID: session.ID,
			Existing:  true,
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
		slog.Int("num_outbox", len(outbox)),
	)

	err = b.persistSessionState(ctx)
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

// findOutgoingSessionByIdempotencyKey returns the existing outgoing session
// created for the supplied caller intent key, when one is locally known.
func (b *oorDurableBehavior) findOutgoingSessionByIdempotencyKey(
	ctx context.Context, idempotencyKey string) (SessionID, bool, error) {

	if idempotencyKey == "" {
		return SessionID{}, false, nil
	}

	if b.cfg.SessionStore != nil {
		sessionID, found, err := b.cfg.SessionStore.
			FindOutgoingByIdempotencyKey(
				ctx, idempotencyKey,
			)
		if err != nil {
			return SessionID{}, false, fmt.Errorf("find OOR "+
				"idempotency key: %w", err)
		}
		if found {
			return sessionID, true, nil
		}
	}

	for sessionID, handle := range b.sessions {
		if handle == nil || handle.kind != sessionKindOutgoing {
			continue
		}

		if handle.IdempotencyKey == idempotencyKey {
			return sessionID, true, nil
		}
	}

	return SessionID{}, false, nil
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
		slog.String("event_type", fmt.Sprintf("%T", req.Event)),
	)

	handle, ok := b.sessions[req.SessionID]
	if !ok {
		incoming, isIncoming := req.Event.(*IncomingTransferEvent)
		if !isIncoming {
			return fn.Err[ActorResp](
				fmt.Errorf("unknown session: %s",
					req.SessionID),
			)
		}

		err := b.handleIncomingTransfer(ctx, req.SessionID, incoming)
		if err != nil {
			return fn.Err[ActorResp](err)
		}

		return fn.Ok[ActorResp](&DriveEventResponse{})
	}

	if submitAccepted, ok := req.Event.(*SubmitAcceptedEvent); ok {
		err := validateSubmitAcceptedIdentity(
			req.SessionID, submitAccepted,
		)
		if err != nil {
			return fn.Err[ActorResp](err)
		}
	}

	alreadyApplied, err := b.alreadyAppliedSigningDriveEvent(
		ctx, req.SessionID, handle, req.Event,
	)
	if err != nil {
		return fn.Err[ActorResp](err)
	}
	if alreadyApplied {
		return fn.Ok[ActorResp](&DriveEventResponse{})
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
	handle.applyRetryEvent(req.Event)

	b.notifyMaterializedVTXOs(ctx, req.Event)

	if finalizeState == nil {
		err = b.persistSessionState(ctx)
		if err != nil {
			return fn.Err[ActorResp](err)
		}

		if b.cfg.UseSQLEffects {
			if metadata, ok := req.Event.(*IncomingMetadataResolvedEvent); ok {
				err = b.persistIncomingMetadataEffect(
					ctx, req.SessionID, metadata,
				)
				if err != nil {
					return fn.Err[ActorResp](err)
				}
			}
		}

		err = b.driveOutbox(ctx, req.SessionID, handle, outbox)
		if err != nil {
			return fn.Err[ActorResp](err)
		}

		return fn.Ok[ActorResp](&DriveEventResponse{})
	}

	// FinalizeAcceptedEvent emits MarkInputsSpentRequest, which may cross
	// from this local actor into the VTXO manager. Drive that local
	// completion before taking this actor's package-store write lock so
	// SQLite backends do not deadlock on two actor transactions.
	err = b.driveOutbox(ctx, req.SessionID, handle, outbox)
	if err != nil {
		return fn.Err[ActorResp](err)
	}

	err = b.persistOutgoingPackage(ctx, req.SessionID, finalizeState)
	if err != nil {
		return fn.Err[ActorResp](err)
	}

	err = b.persistSessionState(ctx)
	if err != nil {
		return fn.Err[ActorResp](err)
	}

	// Emit the outgoing-transfer ledger entry only after the
	// session state commits: the caller contract for the ledger
	// actor is that we have a durable local record of the send
	// before posting to accounting, so a crash before session persistence
	// does not double-book the transfers_out leg on
	// replay.
	b.emitVTXOSent(ctx, req.SessionID, finalizeState)

	return fn.Ok[ActorResp](&DriveEventResponse{})
}

// alreadyAppliedSigningDriveEvent reports whether a durable signing effect
// replay delivered a result for a signing stage the session has already left.
// Signing-effect messages can be retried independently of the OOR actor. The
// OOR FSM must therefore treat stale duplicate signing results as no-ops once
// the corresponding signing event has already advanced the session.
func (b *oorDurableBehavior) alreadyAppliedSigningDriveEvent(
	ctx context.Context, sessionID SessionID, handle *sessionHandle,
	event Event) (bool, error) {

	if handle.kind != sessionKindOutgoing {
		return false, nil
	}

	state, err := handle.currentOutgoingState()
	if err != nil {
		return false, err
	}

	if !isStaleSigningEvent(state, event) {
		return false, nil
	}

	if err := b.recoverStaleSigningSideEffect(
		ctx, sessionID, handle, state, event,
	); err != nil {
		return false, err
	}

	b.logger(ctx).DebugS(ctx, "Ignoring stale signing event",
		slog.String("state", fmt.Sprintf("%T", state)),
		slog.String("event_type", fmt.Sprintf("%T", event)),
	)

	return true, nil
}

func (b *oorDurableBehavior) recoverStaleSigningSideEffect(ctx context.Context,
	sessionID SessionID, handle *sessionHandle, state State,
	event Event) error {

	var outbox []OutboxEvent

	switch event.(type) {
	case *ArkSignedEvent:
		submitState, ok := state.(*AwaitingSubmitAccepted)
		if !ok {
			return nil
		}

		outbox = []OutboxEvent{
			&SendSubmitPackageRequest{
				ArkPSBT:         submitState.ArkPSBT,
				CheckpointPSBTs: submitState.CheckpointPSBTs,
				TransferInputs:  submitState.TransferInputs,
				Recipients:      submitState.RecipientOutputs,
			},
		}

	case *CheckpointsSignedEvent:
		finalizeState, ok := state.(*AwaitingFinalizeAccepted)
		if !ok {
			return nil
		}

		outbox = []OutboxEvent{
			&SendFinalizePackageRequest{
				ArkPSBT: finalizeState.ArkPSBT,
				FinalCheckpointPSBTs: finalizeState.
					FinalCheckpointPSBTs,
			},
		}

	default:
		return nil
	}

	b.logger(ctx).DebugS(ctx, "Recovering stale signing side effect",
		slog.String("session_id", sessionID.String()),
		slog.String("state", fmt.Sprintf("%T", state)),
		slog.String("event_type", fmt.Sprintf("%T", event)),
	)

	if err := b.persistSessionState(ctx); err != nil {
		return err
	}

	return b.driveOutbox(ctx, sessionID, handle, outbox)
}

func isStaleSigningEvent(state State, event Event) bool {
	switch evt := event.(type) {
	case *ArkSignedEvent:
		return isPastArkSigningState(state)

	case *CheckpointsSignedEvent:
		return isPastCheckpointSigningState(state)

	case *OutboxErrorEvent:
		switch evt.OutboxType {
		case (&RequestArkSignatures{}).outboxType():
			return isPastArkSigningState(state) ||
				isTerminalSigningFailure(state)

		case (&RequestCheckpointSignatures{}).outboxType():
			return isPastCheckpointSigningState(state) ||
				isTerminalSigningFailure(state)

		default:
			return false
		}

	default:
		return false
	}
}

func isPastArkSigningState(state State) bool {
	switch state.(type) {
	case *AwaitingSubmitAccepted, *AwaitingCheckpointSignatures,
		*AwaitingFinalizeAccepted, *AwaitingLocalVTXOUpdate,
		*Completed:
		return true

	default:
		return false
	}
}

func isPastCheckpointSigningState(state State) bool {
	switch state.(type) {
	case *AwaitingFinalizeAccepted, *AwaitingLocalVTXOUpdate, *Completed:
		return true

	default:
		return false
	}
}

func isTerminalSigningFailure(state State) bool {
	_, ok := state.(*Failed)

	return ok
}

// handleResolveIncomingTransfer durably records a lightweight incoming OOR
// hint, then emits the transport query needed to resolve the full Ark package
// after the session state commits.
func (b *oorDurableBehavior) handleResolveIncomingTransfer(ctx context.Context,
	req *ResolveIncomingTransferRequest) fn.Result[ActorResp] {

	if req == nil {
		return fn.Err[ActorResp](fmt.Errorf("request must be provided"))
	}

	if len(req.RecipientPkScript) == 0 {
		return fn.Err[ActorResp](
			fmt.Errorf("recipient pk script must be provided"),
		)
	}

	b.logger(ctx).DebugS(ctx, "Handling incoming transfer hint",
		slog.String("session_id", req.SessionID.String()),
		slog.Uint64("recipient_event_id", req.RecipientEventID),
		slog.String(
			"recipient_pk_script",
			hex.EncodeToString(req.RecipientPkScript),
		))

	created := false
	handle, ok := b.sessions[req.SessionID]
	if ok && handle.kind != sessionKindIncoming {
		state, err := handle.currentSessionState()
		if err != nil {
			return fn.Err[ActorResp](err)
		}

		outgoingState, isOutgoingState := state.(State)
		if !isOutgoingState {
			return fn.Err[ActorResp](
				fmt.Errorf("session %s has unexpected "+
					"outgoing state type %T",
					req.SessionID, state),
			)
		}

		if !outgoingState.IsTerminal() {
			b.logger(ctx).DebugS(
				ctx,
				"Persisting incoming self-transfer hint "+
					"until outgoing session reaches "+
					"terminal state",
				slog.String(
					"session_id", req.SessionID.String(),
				),
				slog.String("state", fmt.Sprintf("%T", state)),
			)

			b.stagePendingIncoming(req)
			if err := b.persistSessionState(ctx); err != nil {
				return fn.Err[ActorResp](err)
			}

			return fn.Ok[ActorResp](&DriveEventResponse{})
		}

		b.logger(ctx).DebugS(
			ctx, "Replacing terminal outgoing session "+
				"with incoming self-transfer session",
			slog.String("session_id", req.SessionID.String()),
			slog.String("state", fmt.Sprintf("%T", state)),
		)

		delete(b.sessions, req.SessionID)
		delete(b.pendingIncoming, req.SessionID)
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
		delete(b.pendingIncoming, req.SessionID)
		created = true

		err = b.persistSessionState(ctx)
		if err != nil {
			return fn.Err[ActorResp](err)
		}
	}

	if handle.kind != sessionKindIncoming {
		return fn.Err[ActorResp](
			fmt.Errorf("session %s is not incoming", req.SessionID),
		)
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

func (b *oorDurableBehavior) stagePendingIncoming(
	req *ResolveIncomingTransferRequest) {

	if b.pendingIncoming == nil {
		b.pendingIncoming = make(
			map[SessionID]*ResolveIncomingTransferRequest,
		)
	}

	b.pendingIncoming[req.SessionID] = cloneResolveIncomingTransferRequest(
		req,
	)
}

func cloneResolveIncomingTransferRequest(
	req *ResolveIncomingTransferRequest) *ResolveIncomingTransferRequest {

	if req == nil {
		return nil
	}

	return &ResolveIncomingTransferRequest{
		SessionID:         req.SessionID,
		RecipientEventID:  req.RecipientEventID,
		RecipientPkScript: append([]byte(nil), req.RecipientPkScript...),
	}
}

func (b *oorDurableBehavior) promotePendingIncomingIfTerminal(
	ctx context.Context, sessionID SessionID) error {

	req, ok := b.pendingIncoming[sessionID]
	if !ok {
		return nil
	}

	handle, ok := b.sessions[sessionID]
	if !ok || handle == nil || handle.kind != sessionKindOutgoing {
		return nil
	}

	state, err := handle.currentSessionState()
	if err != nil {
		return err
	}

	outgoingState, ok := state.(State)
	if !ok {
		return fmt.Errorf("session %s has unexpected outgoing "+
			"state type %T", sessionID, state)
	}

	if !outgoingState.IsTerminal() {
		return nil
	}

	b.logger(ctx).DebugS(
		ctx,
		"Promoting pending incoming self-transfer hint",
		slog.String("session_id", sessionID.String()),
		slog.String("state", fmt.Sprintf("%T", state)),
	)

	result := b.handleResolveIncomingTransfer(ctx, req)
	if result.IsErr() {
		return result.Err()
	}

	return nil
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
		return fmt.Errorf("session %s already exists, rejecting "+
			"incoming transfer", sessionID)
	}

	if event.SessionID != (SessionID{}) && event.SessionID != sessionID {
		return fmt.Errorf("incoming event session id mismatch")
	}

	b.logger(ctx).DebugS(ctx, "Handling incoming transfer event",
		slog.String("session_id", sessionID.String()),
		slog.Int("num_checkpoints", len(event.FinalCheckpointPSBTs)),
	)

	session, outbox, err := DriveIncomingTransferWithCheckpoints(
		ctx, sessionID, event.ArkPSBT, event.FinalCheckpointPSBTs,
		event.AncestorPackages,
	)
	if err != nil {
		return err
	}

	b.logger(ctx).DebugS(ctx, "Incoming transfer produced outbox",
		slog.String("session_id", session.ID.String()),
		slog.Int("outbox_len", len(outbox)),
	)

	handle := &sessionHandle{
		FSM:  session.FSM,
		kind: sessionKindIncoming,
	}
	b.sessions[session.ID] = handle

	err = b.persistSessionState(ctx)
	if err != nil {
		return err
	}

	return b.driveOutbox(ctx, session.ID, handle, outbox)
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
		slog.Int("num_checkpoints", len(state.FinalCheckpointPSBTs)),
	)

	err := b.cfg.PackageStore.UpsertPackage(
		ctx, PackageDirectionOutgoing, sessionHash, state.ArkPSBT,
		state.FinalCheckpointPSBTs,
	)
	if err != nil {
		return err
	}

	outpoints := InputOutpoints(state.TransferInputs)
	for i := range outpoints {
		err := b.cfg.PackageStore.UpsertBinding(
			ctx, outpoints[i], sessionHash, uint32(i),
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
					slog.String(
						"session_id",
						sessionID.String(),
					),
					slog.String(
						"outpoint",
						outpoints[i].String(),
					),
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
			b.logger(ctx).WarnS(
				ctx,
				"Failed to emit VTXOSentMsg to ledger",
				err,
				slog.String("session_id", sessionID.String()),
				slog.Int64("amount_sat", total),
			)
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
					slog.String(
						"outpoint",
						desc.Outpoint.String(),
					),
					slog.Int64(
						"amount_sat",
						int64(desc.Amount),
					))
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
		slog.String("session_id", req.Snapshot.SessionID.String()),
	)

	if _, exists := b.sessions[req.Snapshot.SessionID]; exists {
		return fn.Err[ActorResp](
			fmt.Errorf("duplicate session id during restore: %s",
				req.Snapshot.SessionID),
		)
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

	err = b.persistSessionState(ctx)
	if err != nil {
		return fn.Err[ActorResp](err)
	}

	b.logger(ctx).InfoS(ctx, "Session restored successfully",
		slog.String("session_id", session.ID.String()),
	)

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
		slog.String("session_id", req.SessionID.String()),
	)

	handle, ok := b.sessions[req.SessionID]
	if !ok {
		return fn.Err[ActorResp](
			fmt.Errorf("unknown session: %s", req.SessionID),
		)
	}

	state, err := handle.currentSessionState()
	if err != nil {
		return fn.Err[ActorResp](err)
	}

	b.logger(ctx).DebugS(ctx, "Session current state for resume",
		slog.String("session_id", req.SessionID.String()),
		slog.String("state", fmt.Sprintf("%T", state)),
	)

	outbox, err := outboxForHandle(handle, state)
	if err != nil {
		return fn.Err[ActorResp](err)
	}
	handle.clearRetryMetadata()

	err = b.persistSessionState(ctx)
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
		return fn.Err[ActorResp](
			fmt.Errorf("unknown session: %s", req.SessionID),
		)
	}

	if handle.kind != sessionKindOutgoing {
		return fn.Err[ActorResp](
			fmt.Errorf("export snapshot only supports outgoing " +
				"sessions"),
		)
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
		return fn.Err[ActorResp](
			fmt.Errorf("unknown session: %s", req.SessionID),
		)
	}

	state, err := handle.currentSessionState()
	if err != nil {
		return fn.Err[ActorResp](err)
	}

	return fn.Ok[ActorResp](&GetStateResponse{
		State: state,
	})
}

func resolveRequestFromIncomingSnapshot(
	snapshot *IncomingSnapshot) *ResolveIncomingTransferRequest {

	if snapshot == nil {
		return nil
	}

	return &ResolveIncomingTransferRequest{
		SessionID:        snapshot.SessionID,
		RecipientEventID: snapshot.RecipientEventID,
		RecipientPkScript: append(
			[]byte(nil), snapshot.RecipientPkScript...,
		),
	}
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
		slog.Int("num_sessions", len(sessionIDs)),
	)

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
			slog.Int("num_outbox", len(outbox)),
		)

		err = b.driveOutbox(ctx, sessionID, handle, outbox)
		if err != nil {
			return err
		}

		err = b.promotePendingIncomingIfTerminal(ctx, sessionID)
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

// buildTransportMessage wraps the outbox message into the serverconn message
// that should be durably delivered to the server transport actor.
func (b *oorDurableBehavior) buildTransportMessage(ctx context.Context,
	msg OutboxEvent) (serverconn.ServerConnMsg, error) {

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

		return sendReq, nil

	case *QueryIncomingMetadataRequest:
		recipients := queryReq.Recipients
		filter, ok := b.cfg.OutboxHandler.(incomingMetadataFilter)
		if ok {
			var err error
			owned, err := filter.FilterIncomingMetadataRecipients(
				ctx, queryReq.Recipients,
			)
			if err != nil {
				return nil, fmt.Errorf("filter incoming "+
					"metadata recipients: %w", err)
			}

			recipients = owned
		}

		if len(recipients) == 0 {
			return nil, fmt.Errorf("incoming metadata query " +
				"contains no wallet-owned recipients")
		}

		pkScripts := make([][]byte, 0, len(recipients))
		for i := range recipients {
			pkScripts = append(
				pkScripts,
				append(
					[]byte(nil), recipients[i].PkScript...,
				),
			)
		}

		// The durable receive FSM performs a single bounded metadata
		// lookup. It does not page through NextCursor or re-emit this
		// query.
		sendReq := &serverconn.SendListVTXOsByScriptsRequest{
			PkScripts: pkScripts,
			Limit:     b.cfg.Limits.MaxVTXOMatches,
			CorrelationID: IncomingMetadataCorrelationID(
				queryReq.SessionID,
			),
		}

		return sendReq, nil
	}

	if !ok {
		return nil, fmt.Errorf("transport event %T does not implement "+
			"ServerMessage", msg)
	}

	sm := serverMsg.ServiceMethod()
	sendReq := &serverconn.SendClientEventRequest{
		Message: serverMsg,
		Service: sm.Service,
		Method:  sm.Method,
	}

	return sendReq, nil
}

// sendTransportEvent wraps the outbox message in a serverconn request and
// Tell's it to the SQL-backed transport boundary.
func (b *oorDurableBehavior) sendTransportEvent(ctx context.Context,
	msg OutboxEvent) error {

	sendReq, err := b.buildTransportMessage(ctx, msg)
	if err != nil {
		return err
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

	if b.cfg.UseSQLEffects {
		return nil
	}

	return b.driveOutboxNow(ctx, sessionID, handle, outbox)
}

// driveOutboxNow executes outbox work immediately. The SQL effect worker
// uses this after claiming a durable effect row.
func (b *oorDurableBehavior) driveOutboxNow(ctx context.Context,
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
			b.logger(ctx).DebugS(
				ctx,
				"Sending transport event to server",
				slog.String("session_id", sessionID.String()),
				slog.String(
					"event_type", fmt.Sprintf("%T", msg),
				),
			)

			if err := b.sendTransportEvent(ctx, msg); err != nil {
				return err
			}

			if _, ok := msg.(*SendIncomingAckRequest); ok {
				nextOutbox, err := b.askEvent(
					ctx, handle.FSM,
					&IncomingAckSentEvent{},
				)
				if err != nil {
					return err
				}

				err = b.persistSessionState(ctx)
				if err != nil {
					return err
				}

				err = b.driveOutboxNow(
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
			slog.String("event_type", fmt.Sprintf("%T", msg)),
		)

		if handler == nil {
			return fmt.Errorf("outbox handler must be provided " +
				"for local events")
		}

		// Local events (signing, persistence, timers) continue
		// through the outbox handler.
		followUps, err := handler.Handle(ctx, sessionID, msg)
		if err != nil {
			b.logger(ctx).WarnS(
				ctx,
				"Outbox handler error, wrapping as retryable "+
					"event",
				err,
				slog.String("session_id", sessionID.String()),
				slog.String(
					"event_type", fmt.Sprintf("%T", msg),
				),
			)

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

			if finalizeState == nil {
				err = b.persistSessionState(ctx)
				if err != nil {
					return err
				}

				err = b.driveOutboxNow(
					ctx, sessionID, handle, nextOutbox,
				)
				if err != nil {
					return err
				}

				err = b.promotePendingIncomingIfTerminal(
					ctx, sessionID,
				)
				if err != nil {
					return err
				}

				continue
			}

			// FinalizeAcceptedEvent emits local input-spend work.
			// Run it before this actor writes the outgoing package.
			// This keeps SQLite from holding two contending actor
			// transactions.
			err = b.driveOutboxNow(
				ctx, sessionID, handle, nextOutbox,
			)
			if err != nil {
				return err
			}

			err = b.persistOutgoingPackage(
				ctx, sessionID, finalizeState,
			)
			if err != nil {
				return err
			}

			err = b.persistSessionState(ctx)
			if err != nil {
				return err
			}

			err = b.promotePendingIncomingIfTerminal(ctx, sessionID)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func (b *oorDurableBehavior) processSQLClientEffect(ctx context.Context,
	effect OORClientEffect) error {

	handle, ok := b.sessions[effect.SessionID]
	if !ok {
		return fmt.Errorf("unknown session for OOR client effect: %s",
			effect.SessionID)
	}

	state, err := handle.currentSessionState()
	if err != nil {
		return err
	}

	if effect.EffectType == OORClientEffectMaterializeIncomingVTXOs {
		store, ok := b.cfg.SessionStore.(OORClientIncomingEffectStore)
		if !ok {
			return fmt.Errorf("session store cannot build " +
				"incoming materialization request")
		}

		req, err := store.BuildMaterializeIncomingVTXOsRequest(
			ctx, effect.SessionID,
		)
		if err != nil {
			return err
		}

		b.logger(ctx).DebugS(ctx, "Processing SQL OOR client effect",
			slog.String("session_id", effect.SessionID.String()),
			slog.String("effect_id", effect.ID),
			slog.String("effect_type", effect.EffectType),
		)

		return b.driveOutboxNow(
			ctx, effect.SessionID, handle, []OutboxEvent{
				req,
			},
		)
	}

	outbox, err := outboxForHandle(handle, state)
	if err != nil {
		return err
	}

	for _, msg := range outbox {
		effectType, ok := EffectTypeForOutbox(msg)
		if !ok || effectType != effect.EffectType {
			continue
		}

		b.logger(ctx).DebugS(ctx, "Processing SQL OOR client effect",
			slog.String("session_id", effect.SessionID.String()),
			slog.String("effect_id", effect.ID),
			slog.String("effect_type", effect.EffectType),
		)

		err = b.driveOutboxNow(
			ctx, effect.SessionID, handle, []OutboxEvent{
				msg,
			},
		)
		if err != nil {
			return err
		}
		if b.isTransportEvent(msg) {
			return ErrOORClientEffectAwaitingExternalAck
		}

		return nil
	}

	b.logger(ctx).DebugS(ctx, "Skipping stale SQL OOR client effect",
		slog.String("session_id", effect.SessionID.String()),
		slog.String("effect_id", effect.ID),
		slog.String("effect_type", effect.EffectType),
		slog.String("state", fmt.Sprintf("%T", state)),
	)

	return nil
}

func (b *oorDurableBehavior) persistIncomingMetadataEffect(ctx context.Context,
	sessionID SessionID, event *IncomingMetadataResolvedEvent) error {

	if event == nil {
		return nil
	}

	store, ok := b.cfg.SessionStore.(OORClientIncomingEffectStore)
	if !ok {
		return fmt.Errorf("session store cannot persist incoming " +
			"metadata effect")
	}

	return store.SaveIncomingMetadataEffect(ctx, sessionID, event.Matches)
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

	if b.cfg.IncomingVTXOObserver != nil {
		err := b.cfg.IncomingVTXOObserver(ctx, descs)
		if err != nil {
			b.logger(ctx).WarnS(
				ctx,
				"Failed to notify incoming VTXO observer",
				err,
				slog.Int("num_vtxos", len(descs)),
			)
		}
	}

	b.emitVTXOsReceived(ctx, descs)
}

// loadMaterializedVTXOs reloads persisted incoming VTXO descriptors for a
// callback event that only round-tripped outpoint identifiers through the
// SQL mailbox.
func (b *oorDurableBehavior) loadMaterializedVTXOs(ctx context.Context,
	handled *IncomingHandledEvent) []*vtxo.Descriptor {

	if handled == nil || len(handled.MaterializedOutpoints) == 0 {
		return nil
	}

	if b.cfg.VTXOStore == nil {
		b.logger(ctx).WarnS(
			ctx, "Missing VTXO store for incoming callback reload",
			nil,
			slog.Int(
				"num_outpoints",
				len(handled.MaterializedOutpoints),
			))

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

// persistSessionState snapshots every active session into the OOR SQL session
// store.
func (b *oorDurableBehavior) persistSessionState(ctx context.Context) error {
	return b.persistSessionsToStore(ctx)
}

func (b *oorDurableBehavior) persistSessionsToStore(ctx context.Context) error {
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

			if err := b.cfg.SessionStore.SaveOutgoingSession(
				ctx, snapshot,
			); err != nil {
				return err
			}

		case sessionKindIncoming:
			snapshot, err := NewIncomingSnapshot(sessionID, state)
			if err != nil {
				return err
			}

			if err := b.cfg.SessionStore.SaveIncomingSession(
				ctx, snapshot,
			); err != nil {
				return err
			}

		default:
			return fmt.Errorf("unknown session kind: %d",
				handle.kind)
		}
	}

	pendingIncomingIDs := make(
		[]SessionID, 0, len(b.pendingIncoming),
	)
	for sessionID := range b.pendingIncoming {
		pendingIncomingIDs = append(pendingIncomingIDs, sessionID)
	}

	sort.SliceStable(pendingIncomingIDs, func(i, j int) bool {
		return pendingIncomingIDs[i].String() <
			pendingIncomingIDs[j].String()
	})

	for i := range pendingIncomingIDs {
		sessionID := pendingIncomingIDs[i]
		if handle, ok := b.sessions[sessionID]; ok &&
			handle.kind == sessionKindIncoming {

			continue
		}

		if err := b.cfg.SessionStore.SavePendingIncomingHint(
			ctx, b.pendingIncoming[sessionID],
		); err != nil {
			return err
		}
	}

	return nil
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
		return nil, fmt.Errorf("unexpected outgoing state type: %T",
			state)
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
			return nil, fmt.Errorf("unexpected outgoing state "+
				"type: %T", state)
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
func (b *oorDurableBehavior) resumeOutboxForHandle(handle *sessionHandle,
	state SessionState) ([]OutboxEvent, error) {

	if handle == nil {
		return nil, fmt.Errorf("session handle must be provided")
	}

	switch handle.kind {
	case sessionKindOutgoing:
		outgoingState, ok := state.(State)
		if !ok {
			return nil, fmt.Errorf("unexpected outgoing state "+
				"type: %T", state)
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
		return nil, fmt.Errorf("unknown session kind: %d", handle.kind)
	}
}

type durableBehaviorIface = actor.ActorBehavior[
	ActorMsg, ActorResp,
]

var _ durableBehaviorIface = (*oorDurableBehavior)(nil)
