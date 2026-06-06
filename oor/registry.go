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
	IncomingHandler      OutboxHandler
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

// OORRegistryActor is the thin coordinator over per-session OOR actors. It
// owns admission, dedup, restore, and routing; it is registered under the OOR
// service key so every OOR message lands here and is fanned out to the right
// per-session child. The registry's inbound mailbox is durable: a server-push
// event is persisted before the ingress loop advances its ack watermark, so a
// crash between ingress and the per-session child can never lose the event --
// the registry turn (spawn + forward) is simply replayed. The hot receive path
// (DriveEventRequest) is forwarded to a child via Tell, so the registry's
// single goroutine never blocks on child processing and independent sessions
// stay parallel.
type OORRegistryActor struct {
	ref      actor.ActorRef[OORDurableMsg, ActorResp]
	registry *actor.DurableActor[OORDurableMsg, ActorResp]
	behavior *oorRegistryBehavior
}

// Ref returns the public registry actor reference.
func (a *OORRegistryActor) Ref() actor.ActorRef[OORDurableMsg, ActorResp] {
	return a.ref
}

// Stop stops the registry and all active children. The Read/Stage/Commit
// durable path has no Stoppable hook, so the children are stopped here, after
// the registry goroutine has exited and can no longer touch the active set.
func (a *OORRegistryActor) Stop() {
	if a == nil || a.registry == nil {
		return
	}

	a.registry.Stop()
	a.behavior.stopChildren()
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

	// The registry reuses the legacy global actor's mailbox id, so any
	// unacked rows left by a pre-cutover deployment are drained through
	// the same routing surface after an upgrade.
	durableCfg := actor.DefaultDurableTxActorConfig[
		OORDurableMsg, ActorResp, oorTx,
	](
		OORActorServiceKeyName, behavior, behavior.bindStores,
		cfg.DeliveryStore, newOORActorCodec(cfg.Limits),
	)
	durableCfg.Log = cfg.Log

	registry, err := actor.NewDurableActor(durableCfg).Unpack()
	if err != nil {
		return nil, fmt.Errorf("create oor registry actor: %w", err)
	}
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

// RestoreNonTerminal respawns and resumes a per-session actor for every
// non-terminal registry row so in-flight sessions make progress after a
// restart. The restore runs as a registry message so the active set is only
// ever touched on the registry goroutine, serialized with any backlog the
// durable inbox redelivers at boot.
func (a *OORRegistryActor) RestoreNonTerminal(ctx context.Context) error {
	if a == nil {
		return fmt.Errorf("registry actor not initialized")
	}

	res := a.ref.Ask(ctx, &RestoreNonTerminalRequest{}).Await(ctx)
	if res.IsErr() {
		return res.Err()
	}

	return nil
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

// bindStores is the StoreFactory for the registry's Read/Stage/Commit path.
func (r *oorRegistryBehavior) bindStores(_ context.Context,
	ds actor.DeliveryStore) oorTx {

	return oorTx{
		store:   ds,
		actorID: OORActorServiceKeyName,
	}
}

// stopChildren stops every active child actor. Called by the registry wrapper
// after the registry goroutine has exited.
func (r *oorRegistryBehavior) stopChildren() {
	for _, child := range r.active {
		child.Stop()
	}
}

// Compile-time check that the registry runs on the Read/Stage/Commit path.
var _ actor.TxBehavior[
	OORDurableMsg, ActorResp, oorTx,
] = (*oorRegistryBehavior)(nil)

// Receive fans one OOR message out to the right per-session child. Read-only
// probes return directly and are consumed via the framework's
// non-transactional ack; routing and admission turns are consumed by one
// lease-fenced empty Commit after the forward has been durably enqueued in
// the target child's mailbox, so a crash mid-turn replays the spawn+forward
// rather than losing the message.
func (r *oorRegistryBehavior) Receive(ctx context.Context, msg OORDurableMsg,
	ax actor.Exec[oorTx]) fn.Result[ActorResp] {

	switch m := msg.(type) {
	case *actor.RestartMessage:
		// The active set is rebuilt by RestoreNonTerminal; the restart
		// message carries no state to persist.
		return fn.Ok[ActorResp](&DriveEventResponse{})

	case *GetStateRequest:
		return r.routeAsk(ctx, m.SessionID, m)

	case *ListSessionsRequest:
		return r.handleListSessions(ctx, m)
	}

	res := r.dispatch(ctx, msg)
	if res.IsErr() {
		return res
	}

	// Consume the routing turn: the forward (if any) is already persisted
	// in the child's durable mailbox, so the empty Commit folds this
	// message's lease-fenced ack and dedup mark into one short
	// transaction. A crash before this point redelivers the message and
	// replays the idempotent spawn+forward.
	err := ax.Commit(ctx, func(context.Context, oorTx) error {
		return nil
	})
	if err != nil {
		return fn.Err[ActorResp](err)
	}

	return res
}

// dispatch routes one state-bearing registry message to its handler.
func (r *oorRegistryBehavior) dispatch(ctx context.Context,
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

	case *RestoreNonTerminalRequest:
		return r.handleRestoreNonTerminal(ctx, m)

	default:
		return fn.Err[ActorResp](
			fmt.Errorf("unknown oor registry message: %T", msg),
		)
	}
}

// handleRestoreNonTerminal runs the boot-time restore on the registry
// goroutine.
func (r *oorRegistryBehavior) handleRestoreNonTerminal(ctx context.Context,
	_ *RestoreNonTerminalRequest) fn.Result[ActorResp] {

	if err := r.restoreNonTerminal(ctx); err != nil {
		return fn.Err[ActorResp](err)
	}

	return fn.Ok[ActorResp](&DriveEventResponse{})
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

	// Hand the admission to the child without parking the registry
	// goroutine on the child's signing and commit: the Ask persists the
	// request in the child's durable mailbox, the caller's detached
	// promise is settled by the child's result, and the registry's turn
	// ends as soon as the forward is durable. Admission errors still reach
	// the caller inline through the continuation.
	future := child.Ref().Ask(ctx, req)

	detachedAsk, ok := actor.DetachAskPromise[ActorResp](ctx)
	if !ok {
		// No detachable caller (a redelivered turn, or a test driving
		// the behavior directly): preserve the synchronous semantics.
		res := future.Await(ctx)
		if res.IsErr() {
			// A failed admission turn commits no durable row, so
			// keeping the child registered would turn the
			// in-memory dedup into a phantom: a retry of the same
			// transfer would be told Existing for a session with
			// no durable backing. Drop the freshly spawned child
			// so the retry admits cleanly. The active-map dedup
			// above guarantees this child was spawned by this
			// turn.
			r.dropChild(sessionID, child)
		}

		return res
	}

	selfRef := r.selfRef

	//nolint:contextcheck // the continuation outlives the turn context;
	// the caller's context bounds the wait and the cleanup Tell is
	// daemon-owned
	future.OnComplete(
		detachedAsk.CallerCtx, func(res fn.Result[ActorResp]) {
			// A failed admission leaves a fresh child with no
			// durable row. The continuation runs off the registry
			// goroutine, so route the cleanup through the
			// registry's own mailbox: the reaper treats a missing
			// row as reap-eligible and drops the phantom there.
			if res.IsErr() && selfRef != nil {
				nctx, cancel := context.WithTimeout(
					context.Background(),
					terminalNotifyTimeout,
				)
				defer cancel()

				// The cleanup Tell is best effort: a missed
				// reap only leaves the phantom resident until
				// shutdown.
				_ = selfRef.Tell(
					nctx, &SessionTerminalNotification{
						SessionID: sessionID,
					},
				)
			}

			detachedAsk.Promise.Complete(res)
		},
	)

	return fn.Ok[ActorResp](&StartTransferResponse{SessionID: sessionID})
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

// handleResolveIncoming admits or continues an incoming receive session. The
// session id and pk script arrive from the operator-controlled event stream,
// so admission validates the recipient is wallet-owned before allocating a
// durable per-session actor for it.
func (r *oorRegistryBehavior) handleResolveIncoming(ctx context.Context,
	req *ResolveIncomingTransferRequest) fn.Result[ActorResp] {

	if err := r.validateIncomingAdmission(ctx, req); err != nil {
		return fn.Err[ActorResp](err)
	}

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
		r.dropChild(sessionID, child)
	}

	r.logger(ctx).InfoS(ctx, "Replacing terminal outgoing session with "+
		"incoming self-transfer session",
		slog.String("session_id", sessionID.String()),
	)

	return nil
}

// validateIncomingAdmission rejects incoming hints whose recipient pk script
// the wallet does not own. Without this gate, a misbehaving operator could
// spawn one durable child per fabricated session id; the recipient filter is
// the same ownership check the metadata query applies later, hoisted to the
// admission boundary. A nil or non-filtering IncomingHandler skips the check.
func (r *oorRegistryBehavior) validateIncomingAdmission(ctx context.Context,
	req *ResolveIncomingTransferRequest) error {

	filter, ok := r.cfg.IncomingHandler.(incomingMetadataFilter)
	if !ok {
		return nil
	}

	owned, err := filter.FilterIncomingMetadataRecipients(
		ctx, []ArkRecipientOutput{{
			PkScript: req.RecipientPkScript,
		}},
	)
	if err != nil {
		return fmt.Errorf("validate incoming admission for session "+
			"%s: %w", req.SessionID, err)
	}
	if len(owned) == 0 {
		return fmt.Errorf("incoming recipient pk script not owned by "+
			"wallet: session %s", req.SessionID)
	}

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

	r.dropChild(msg.SessionID, child)

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

// routeAsk forwards a request to a session's child. With a detachable caller
// the child's result settles the promise directly so the registry goroutine
// never parks on the child's turn; otherwise the response is awaited inline.
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

	future := child.Ref().Ask(ctx, msg)

	detachedAsk, ok := actor.DetachAskPromise[ActorResp](ctx)
	if !ok {
		return future.Await(ctx)
	}

	//nolint:contextcheck // the continuation outlives the turn context;
	// the caller's context bounds the wait
	future.OnComplete(
		detachedAsk.CallerCtx, func(res fn.Result[ActorResp]) {
			detachedAsk.Promise.Complete(res)
		},
	)

	return fn.Ok[ActorResp](&DriveEventResponse{})
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

// ensureChild returns the active child for a session, spawning and registering
// it when absent.
func (r *oorRegistryBehavior) ensureChild(sessionID SessionID,
	direction clientdb.OORSessionDirection) (*OORSessionActor, error) {

	if child, ok := r.active[sessionID]; ok {
		return child, nil
	}

	child, err := r.spawnFunc(sessionID, direction)
	if err != nil {
		return nil, err
	}

	// Register the child under its deterministic per-session key so the
	// ingress fast path can tell session-addressed server pushes straight
	// into the child's durable mailbox, skipping the registry hop.
	if r.cfg.ActorSystem != nil {
		err := actor.RegisterWithReceptionist(
			r.cfg.ActorSystem.Receptionist(),
			SessionServiceKey(sessionID), child.Ref(),
		)
		if err != nil {
			child.Stop()

			return nil, fmt.Errorf("register session %s child: %w",
				sessionID, err)
		}
	}

	r.active[sessionID] = child

	return child, nil
}

// dropChild deregisters a child's per-session service key, stops it, and
// removes it from the active set. The deregistration runs first so the ingress
// fast path falls back to the registry while the child winds down.
func (r *oorRegistryBehavior) dropChild(sessionID SessionID,
	child *OORSessionActor) {

	if r.cfg.ActorSystem != nil {
		SessionServiceKey(sessionID).Unregister(
			r.cfg.ActorSystem, child.Ref(),
		)
	}

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
		IncomingHandler:      r.cfg.IncomingHandler,
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
