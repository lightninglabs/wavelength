package oor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/build"
	clientdb "github.com/lightninglabs/darepo-client/db"
	"github.com/lightninglabs/darepo-client/ledger"
	"github.com/lightninglabs/darepo-client/serverconn"
	"github.com/lightninglabs/darepo-client/timeout"
	"github.com/lightninglabs/darepo-client/vtxo"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
)

// OORRegistryConfig configures the OOR registry coordinator. Every field except
// the registry's own wiring is a per-session dependency forwarded verbatim into
// each child actor via childConfig.
type OORRegistryConfig struct {
	Log                  fn.Option[btclog.Logger]
	Signer               input.Signer
	RegistryStore        SessionRegistryStore
	DeliveryStore        actor.DeliveryStore
	ServerConn           actor.TellOnlyRef[serverconn.ServerConnMsg]
	VTXOManager          actor.TellOnlyRef[vtxo.ManagerMsg]
	SpendCompleter       SpendCompleter
	ReservationStore     ReservationStore
	PackageStore         PackagePersistence
	VTXOStore            vtxo.VTXOStore
	LedgerSink           fn.Option[ledger.Sink]
	IncomingVTXOObserver IncomingVTXONotifier
	Limits               ReceiveLimits
	TimeoutActor         actor.TellOnlyRef[timeout.Msg]
	CallbackRef          actor.TellOnlyRef[*timeout.ExpiredMsg]
	ActorSystem          actor.SystemContext
}

// OORRegistryActor is the thin coordinator over per-session OOR actors. It owns
// admission, dedup, restore, and routing; it is registered under the OOR
// service key so every OOR message lands here and is fanned out to the right
// per-session child. The hot receive path (DriveEventRequest) is forwarded to a
// child via Tell, so the registry's single goroutine never blocks on child
// processing and independent sessions stay parallel.
type OORRegistryActor struct {
	ref      actor.ActorRef[OORDurableMsg, ActorResp]
	registry *actor.Actor[OORDurableMsg, ActorResp]
	behavior *oorRegistryBehavior
}

// Ref returns the public registry actor reference.
func (a *OORRegistryActor) Ref() actor.ActorRef[OORDurableMsg, ActorResp] {
	return a.ref
}

// Stop stops the registry and all active children.
func (a *OORRegistryActor) Stop() {
	if a == nil || a.registry == nil {
		return
	}

	a.registry.Stop()
}

// NewOORRegistryActor creates and starts the OOR registry coordinator. Call
// RestoreNonTerminal after construction to respawn in-flight sessions.
func NewOORRegistryActor(cfg OORRegistryConfig) (*OORRegistryActor, error) {
	if cfg.RegistryStore == nil {
		return nil, fmt.Errorf("registry store must be provided")
	}
	if cfg.DeliveryStore == nil {
		return nil, fmt.Errorf("delivery store must be provided")
	}

	behavior := &oorRegistryBehavior{
		cfg:    cfg,
		log:    cfg.Log.UnwrapOr(btclog.Disabled),
		active: make(map[SessionID]*OORSessionActor),
	}
	behavior.spawnFunc = behavior.spawn

	registry := actor.NewActor(actor.ActorConfig[OORDurableMsg, ActorResp]{
		ID:          OORActorServiceKeyName,
		Behavior:    behavior,
		MailboxSize: 64,
	})
	behavior.selfRef = registry.TellRef()
	registry.Start()

	a := &OORRegistryActor{
		ref:      registry.Ref(),
		registry: registry,
		behavior: behavior,
	}

	if cfg.ActorSystem != nil {
		key := NewServiceKey()
		err := actor.RegisterWithReceptionist(
			cfg.ActorSystem.Receptionist(), key, registry.Ref(),
		)
		if err != nil {
			registry.Stop()

			return nil, fmt.Errorf("register oor registry: %w", err)
		}
	}

	return a, nil
}

// RestoreNonTerminal respawns a per-session actor for every non-terminal
// registry row so in-flight sessions resume after a restart.
func (a *OORRegistryActor) RestoreNonTerminal(ctx context.Context) error {
	if a == nil {
		return fmt.Errorf("registry actor not initialized")
	}

	return a.behavior.restoreNonTerminal(ctx)
}

// oorRegistryBehavior is the control-plane behavior around per-session actors.
type oorRegistryBehavior struct {
	cfg    OORRegistryConfig
	log    btclog.Logger
	active map[SessionID]*OORSessionActor

	// selfRef is the registry's own tell-only reference, handed to each
	// child so it can report a terminal commit for reaping.
	selfRef actor.TellOnlyRef[OORDurableMsg]

	// spawnFunc creates one per-session child actor. Overridable in tests.
	spawnFunc func(SessionID, clientdb.OORSessionDirection) (
		*OORSessionActor, error)
}

// logger returns the behavior logger bound to ctx.
func (r *oorRegistryBehavior) logger(ctx context.Context) btclog.Logger {
	if r.log != btclog.Disabled {
		return r.log
	}

	return build.LoggerFromContext(ctx)
}

// OnStop stops every active child actor when the registry shuts down.
func (r *oorRegistryBehavior) OnStop(context.Context) error {
	for _, child := range r.active {
		child.Stop()
	}

	return nil
}

// Receive fans one OOR message out to the right per-session child.
func (r *oorRegistryBehavior) Receive(ctx context.Context,
	msg OORDurableMsg) fn.Result[ActorResp] {

	switch m := msg.(type) {
	case *StartTransferRequest:
		return r.handleStartTransfer(ctx, m)

	case *DriveEventRequest:
		return r.handleDriveEvent(ctx, m)

	case *ResolveIncomingTransferRequest:
		return r.handleResolveIncoming(ctx, m)

	case *ResumeSessionRequest:
		return r.handleResumeSession(ctx, m)

	case *SessionTerminalNotification:
		return r.handleSessionTerminal(ctx, m)

	case *GetStateRequest:
		return r.routeAsk(ctx, m.SessionID, m)

	case *ListSessionsRequest:
		return r.handleListSessions(ctx, m)

	default:
		return fn.Err[ActorResp](
			fmt.Errorf("unknown oor registry message: %T", msg),
		)
	}
}

// handleStartTransfer admits a new outgoing session. The session id is the Ark
// txid, known only after the deterministic package is built, so the registry
// builds the session to learn its id, dedups, spawns the child keyed by that
// id, and forwards the request to it (the child rebuilds the identical session
// deterministically).
func (r *oorRegistryBehavior) handleStartTransfer(ctx context.Context,
	req *StartTransferRequest) fn.Result[ActorResp] {

	// Idempotency dedup against the durable store first. The lookup skips
	// failed rows, so a keyed retry after a failed transfer admits a fresh
	// session instead of echoing the dead one.
	if req.IdempotencyKey != "" {
		store := r.cfg.RegistryStore
		existing, err := store.LookupActiveSessionByIdempotencyKey(
			ctx, req.IdempotencyKey,
		)
		switch {
		case err == nil:
			return fn.Ok[ActorResp](&StartTransferResponse{
				SessionID: SessionID(existing.SessionID),
				Existing:  true,
			})

		case !errors.Is(err, clientdb.ErrOORSessionNotFound):
			return fn.Err[ActorResp](err)
		}
	}

	// Build the deterministic session to learn its id. This FSM is
	// discarded; the spawned child rebuilds the identical one.
	session, _, err := NewSessionWithIdempotencyKey(
		ctx, req.Policy, req.Inputs, req.Recipients, req.IdempotencyKey,
	)
	if err != nil {
		return fn.Err[ActorResp](err)
	}
	sessionID := session.ID

	if _, ok := r.active[sessionID]; ok {
		return fn.Ok[ActorResp](&StartTransferResponse{
			SessionID: sessionID,
			Existing:  true,
		})
	}

	child, err := r.ensureChild(
		sessionID, clientdb.OORSessionDirectionOutgoing,
	)
	if err != nil {
		return fn.Err[ActorResp](err)
	}

	res := child.Ref().Ask(ctx, req).Await(ctx)
	if res.IsErr() {
		// A failed admission turn commits no durable row, so keeping
		// the child registered would turn the in-memory dedup into a
		// phantom: a retry of the same transfer would be told
		// Existing for a session with no durable backing. Drop the
		// freshly spawned child so the retry admits cleanly. The
		// active-map dedup above guarantees this child was spawned by
		// this turn.
		r.dropChild(sessionID, child)
	}

	return res
}

// handleDriveEvent routes a follow-up event to its session's child. This is the
// hot receive path, so the child is addressed via Tell (a non-blocking durable
// enqueue) and the registry returns immediately.
func (r *oorRegistryBehavior) handleDriveEvent(ctx context.Context,
	req *DriveEventRequest) fn.Result[ActorResp] {

	child, err := r.lookupOrRestore(ctx, req.SessionID)
	if err != nil {
		return fn.Err[ActorResp](err)
	}
	if child == nil {
		return fn.Err[ActorResp](
			fmt.Errorf("unknown session: %s", req.SessionID),
		)
	}

	if err := child.TellRef().Tell(ctx, req); err != nil {
		return fn.Err[ActorResp](err)
	}

	return fn.Ok[ActorResp](&DriveEventResponse{})
}

// handleResolveIncoming admits or continues an incoming receive session.
func (r *oorRegistryBehavior) handleResolveIncoming(ctx context.Context,
	req *ResolveIncomingTransferRequest) fn.Result[ActorResp] {

	if err := r.resolveSelfTransfer(ctx, req.SessionID); err != nil {
		return fn.Err[ActorResp](err)
	}

	_, existed := r.active[req.SessionID]
	child, err := r.ensureChild(
		req.SessionID, clientdb.OORSessionDirectionIncoming,
	)
	if err != nil {
		return fn.Err[ActorResp](err)
	}

	if err := child.TellRef().Tell(ctx, req); err != nil {
		// A failed forward leaves a freshly spawned child with no
		// durable hint to drive it; drop it so the redelivered
		// admission spawns cleanly. A pre-existing child keeps its
		// state.
		if !existed {
			r.dropChild(req.SessionID, child)
		}

		return fn.Err[ActorResp](err)
	}

	return fn.Ok[ActorResp](&DriveEventResponse{})
}

// resolveSelfTransfer applies the self-transfer invariant before an incoming
// admission: a hint whose session id already exists as an outgoing session
// (recipient == sender, so both sides share the Ark txid) errors until the
// outgoing session reaches a terminal state; then the outgoing entry is
// replaced by a fresh incoming session whose first commit overwrites the
// terminal outgoing row.
func (r *oorRegistryBehavior) resolveSelfTransfer(ctx context.Context,
	sessionID SessionID) error {

	record, err := r.cfg.RegistryStore.GetSession(
		ctx, chainHashOf(sessionID),
	)
	switch {
	case errors.Is(err, clientdb.ErrOORSessionNotFound):
		return nil

	case err != nil:
		return err
	}

	if record.Direction != clientdb.OORSessionDirectionOutgoing {
		return nil
	}

	// The hint is retried by the transport layer, so deferring is just an
	// error until the outgoing session terminates.
	if !record.Status.IsTerminal() {
		r.logger(ctx).DebugS(ctx,
			"Deferring incoming self-transfer hint until "+
				"outgoing session reaches terminal state",
			slog.String("session_id", sessionID.String()),
			slog.String("phase", record.Phase),
		)

		return fmt.Errorf("outgoing session %s still active for "+
			"incoming hint", sessionID)
	}

	// Replace: drop any resident outgoing child so the spawn below
	// installs a fresh incoming session in its place. Terminal children
	// are normally reaped already, so this is a belt-and-braces stop.
	if child, ok := r.active[sessionID]; ok {
		child.Stop()
		delete(r.active, sessionID)
	}

	r.logger(ctx).InfoS(ctx, "Replacing terminal outgoing session with "+
		"incoming self-transfer session",
		slog.String("session_id", sessionID.String()),
	)

	return nil
}

// handleSessionTerminal reaps a child whose session committed a terminal
// snapshot: the durable row is re-checked as the authority, then the child is
// stopped and dropped from the active set. A stale or duplicate notification
// is harmless: an unknown child or a non-terminal row is a no-op.
func (r *oorRegistryBehavior) handleSessionTerminal(ctx context.Context,
	msg *SessionTerminalNotification) fn.Result[ActorResp] {

	child, ok := r.active[msg.SessionID]
	if !ok {
		return fn.Ok[ActorResp](&DriveEventResponse{})
	}

	record, err := r.cfg.RegistryStore.GetSession(
		ctx, chainHashOf(msg.SessionID),
	)
	switch {
	// No durable row means there is nothing left to drive; fall through
	// and reap.
	case errors.Is(err, clientdb.ErrOORSessionNotFound):
	case err != nil:
		return fn.Err[ActorResp](err)

	// The row is authoritative: a notification that races a later
	// non-terminal write (or a replaced session) must not reap a live
	// child.
	case !record.Status.IsTerminal():
		return fn.Ok[ActorResp](&DriveEventResponse{})
	}

	child.Stop()
	delete(r.active, msg.SessionID)

	r.logger(ctx).DebugS(ctx, "Reaped terminal OOR session",
		slog.String("session_id", msg.SessionID.String()),
	)

	return fn.Ok[ActorResp](&DriveEventResponse{})
}

// handleResumeSession routes a retry-timer expiry to its session's child so
// the child re-drives the outbox implied by its current state. A resume for an
// unknown or terminal session is a benign no-op: the timer may fire after the
// session completed or failed, and there is nothing left to re-drive.
func (r *oorRegistryBehavior) handleResumeSession(ctx context.Context,
	req *ResumeSessionRequest) fn.Result[ActorResp] {

	child, err := r.lookupOrRestore(ctx, req.SessionID)
	if err != nil {
		return fn.Err[ActorResp](err)
	}
	if child == nil {
		r.logger(ctx).DebugS(
			ctx,
			"Dropping resume for unknown or terminal session",
			slog.String("session_id", req.SessionID.String()),
		)

		return fn.Ok[ActorResp](&DriveEventResponse{})
	}

	if err := child.TellRef().Tell(ctx, req); err != nil {
		return fn.Err[ActorResp](err)
	}

	return fn.Ok[ActorResp](&DriveEventResponse{})
}

// handleListSessions serves a coarse session list from the control-plane
// store. Terminal rows are included so failed sessions stay visible to status
// RPCs, with the failure reason and the consumed-input projection decoded
// from the persisted snapshot.
func (r *oorRegistryBehavior) handleListSessions(ctx context.Context,
	req *ListSessionsRequest) fn.Result[ActorResp] {

	rows, err := r.cfg.RegistryStore.ListSessions(ctx)
	if err != nil {
		return fn.Err[ActorResp](err)
	}

	summaries := make([]SessionSummary, 0, len(rows))
	for i := range rows {
		dir := SessionDirectionOutgoing
		if rows[i].Direction == clientdb.OORSessionDirectionIncoming {
			dir = SessionDirectionIncoming
		}

		if req.Direction != SessionDirectionAll &&
			req.Direction != dir {

			continue
		}

		pending := !rows[i].Status.IsTerminal()
		if req.PendingOnly && !pending {
			continue
		}

		summary := SessionSummary{
			SessionID:   SessionID(rows[i].SessionID),
			Direction:   dir,
			Phase:       rows[i].Phase,
			Pending:     pending,
			RetryReason: rows[i].LastError,
		}
		r.fillOutgoingSummary(ctx, &summary, &rows[i])

		summaries = append(summaries, summary)
	}

	// The list response is sorted deterministically by session id.
	sort.Slice(summaries, func(i, j int) bool {
		return summaries[i].SessionID.String() <
			summaries[j].SessionID.String()
	})

	return fn.Ok[ActorResp](&ListSessionsResponse{Sessions: summaries})
}

// fillOutgoingSummary projects the consumed-input diagnostics for an outgoing
// session from its persisted snapshot. Decode failures degrade to the coarse
// row fields rather than failing the whole list.
func (r *oorRegistryBehavior) fillOutgoingSummary(ctx context.Context,
	summary *SessionSummary, record *clientdb.OORSessionRegistryRecord) {

	if record.Direction != clientdb.OORSessionDirectionOutgoing ||
		len(record.SnapshotData) == 0 {
		return
	}

	snapshot, err := decodeOutgoingSnapshot(record.SnapshotData)
	if err != nil {
		r.logger(ctx).WarnS(ctx,
			"Failed to decode outgoing snapshot for listing", err,
			slog.String(
				"session_id", summary.SessionID.String(),
			),
		)

		return
	}

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

	if summary.RetryReason == "" {
		summary.RetryReason = snapshot.FailReason
	}
}

// routeAsk forwards a request to a session's child and returns its response.
func (r *oorRegistryBehavior) routeAsk(ctx context.Context, sessionID SessionID,
	msg OORDurableMsg) fn.Result[ActorResp] {

	child, err := r.lookupOrRestore(ctx, sessionID)
	if err != nil {
		return fn.Err[ActorResp](err)
	}
	if child == nil {
		return fn.Err[ActorResp](
			fmt.Errorf("unknown session: %s", sessionID),
		)
	}

	return child.Ref().Ask(ctx, msg).Await(ctx)
}

// lookupOrRestore returns the active child for a session, restoring it from the
// control-plane store when it is non-terminal but not yet in memory. Returns a
// nil child (no error) when the session is unknown.
func (r *oorRegistryBehavior) lookupOrRestore(ctx context.Context,
	sessionID SessionID) (*OORSessionActor, error) {

	if child, ok := r.active[sessionID]; ok {
		return child, nil
	}

	record, err := r.cfg.RegistryStore.GetSession(
		ctx, chainHashOf(sessionID),
	)
	switch {
	case errors.Is(err, clientdb.ErrOORSessionNotFound):
		return nil, nil

	case err != nil:
		return nil, err
	}

	if record.Status.IsTerminal() {
		return nil, nil
	}

	return r.ensureChild(sessionID, record.Direction)
}

// ensureChild returns the active child for a session, spawning it when absent.
func (r *oorRegistryBehavior) ensureChild(sessionID SessionID,
	direction clientdb.OORSessionDirection) (*OORSessionActor, error) {

	if child, ok := r.active[sessionID]; ok {
		return child, nil
	}

	child, err := r.spawnFunc(sessionID, direction)
	if err != nil {
		return nil, err
	}

	r.active[sessionID] = child

	return child, nil
}

// dropChild stops a child actor and removes it from the active set.
func (r *oorRegistryBehavior) dropChild(sessionID SessionID,
	child *OORSessionActor) {

	child.Stop()
	delete(r.active, sessionID)
}

// spawn creates one durable per-session actor from the shared child config.
func (r *oorRegistryBehavior) spawn(sessionID SessionID,
	direction clientdb.OORSessionDirection) (*OORSessionActor, error) {

	return NewOORSessionActor(r.childConfig(sessionID, direction))
}

// childConfig builds the per-session actor config for one session by forwarding
// the shared dependencies and stamping the session identity.
func (r *oorRegistryBehavior) childConfig(sessionID SessionID,
	direction clientdb.OORSessionDirection) SessionActorConfig {

	return SessionActorConfig{
		SessionID:            sessionID,
		Direction:            direction,
		Log:                  r.cfg.Log,
		Signer:               r.cfg.Signer,
		RegistryStore:        r.cfg.RegistryStore,
		DeliveryStore:        r.cfg.DeliveryStore,
		ServerConn:           r.cfg.ServerConn,
		VTXOManager:          r.cfg.VTXOManager,
		SpendCompleter:       r.cfg.SpendCompleter,
		ReservationStore:     r.cfg.ReservationStore,
		PackageStore:         r.cfg.PackageStore,
		VTXOStore:            r.cfg.VTXOStore,
		LedgerSink:           r.cfg.LedgerSink,
		IncomingVTXOObserver: r.cfg.IncomingVTXOObserver,
		Limits:               normalizeReceiveLimits(r.cfg.Limits),
		TimeoutActor:         r.cfg.TimeoutActor,
		CallbackRef:          r.cfg.CallbackRef,
		Registry:             r.selfRef,
	}
}

// restoreNonTerminal spawns a child for every non-terminal control-plane row
// and re-drives each restored session so it makes forward progress instead of
// waiting for an operator response that may never arrive (retry timers are
// in-memory and do not survive a restart).
func (r *oorRegistryBehavior) restoreNonTerminal(ctx context.Context) error {
	rows, err := r.cfg.RegistryStore.ListNonTerminal(ctx)
	if err != nil {
		return err
	}

	for i := range rows {
		sessionID := SessionID(rows[i].SessionID)
		if _, ok := r.active[sessionID]; ok {
			continue
		}

		child, err := r.ensureChild(sessionID, rows[i].Direction)
		if err != nil {
			return fmt.Errorf("restore session %s: %w", sessionID,
				err)
		}

		// Re-emit the outbox implied by the restored state. The Tell
		// lands in the child's durable mailbox, so the resume itself
		// survives a crash between this loop and the child's turn.
		err = child.TellRef().Tell(ctx, &ResumeSessionRequest{
			SessionID: sessionID,
		})
		if err != nil {
			return fmt.Errorf("resume session %s: %w", sessionID,
				err)
		}

		r.logger(ctx).DebugS(ctx, "Restored OOR session",
			slog.String("session_id", sessionID.String()),
		)
	}

	return nil
}
