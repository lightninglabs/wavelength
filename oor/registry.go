package oor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

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

// errIncomingAdmissionCapped is returned when an incoming OOR admission is
// rejected because the daemon already holds the maximum number of resident
// incoming sessions. The hint is retried by transport, so the rejection is
// recoverable once an earlier session terminates and is reaped.
var errIncomingAdmissionCapped = errors.New("incoming OOR admission " +
	"rejected: concurrency cap reached")

// errSelfTransferDeferred is returned when an incoming hint names a session
// that is still active as an outgoing session on this daemon (every payment
// with a change output produces one: the operator pushes the recipient event
// for the sender's own change script under the same Ark-txid session id).
// The registry parks the hint and redrives it when the outgoing session
// reaches a terminal state; the durable delivery is also nacked on a long
// flat backoff as the crash-safety fallback, so this error must stay cheap
// and must never dead-letter while the outgoing session lives.
var errSelfTransferDeferred = errors.New("outgoing session still active for " +
	"incoming hint")

// selfHintRedeliveryBackoff is the flat redelivery backoff for a deferred
// self-transfer hint's durable delivery. The terminal-reap redrive is the
// fast path, so the durable copy only needs to cover a crash or a missed
// redrive; a long flat delay keeps the per-payment defer from amplifying
// write load when the DB writer is already saturated (the feedback loop
// behind the SQLITE_BUSY bursts observed at high payment rates).
const selfHintRedeliveryBackoff = 30 * time.Second

// detachedWaitTimeout bounds the registry's detached-continuation wait on a
// child's response. OnComplete spawns a goroutine that parks on
// DetachedAsk.CallerCtx. On the registry's durable (Read/Stage/Commit) path
// that context is the registry actor's own lifetime context, NOT the
// originating caller's: the durable mailbox does not persist the caller's
// context with the Ask (see DetachedAsk.CallerCtx). It therefore does not
// cancel on a CLI disconnect (the production StartTransfer call site also
// derives its own context from context.WithoutCancel, so even the caller side
// never cancels). Without an upper bound a wedged or never-resolving child turn
// would leak the continuation goroutine for the daemon's lifetime, so this wrap
// is the SOLE bound on the continuation: the registry always wraps CallerCtx in
// this timeout before handing it to OnComplete, and the continuation eventually
// fires (with a deadline error if the child never answered) so the goroutine
// exits. A real caller deadline is observed only by the caller's own
// future.Await, never by this detached continuation.
const detachedWaitTimeout = 5 * time.Minute

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
	SpendReleaser        SpendReleaser
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
// durable path has no Stoppable hook, so the children are stopped here. The
// registry goroutine is drained first via StopAndWait: a redelivered backlog
// turn can mutate the (unsynchronized) active map while it runs, so stopping
// the children before the process loop has exited would race a concurrent
// ensureChild/dropChild against stopChildren's iteration. StopAndWait blocks
// on the actor's done channel, which is closed only after process() returns,
// establishing the happens-before stopChildren relies on.
func (a *OORRegistryActor) Stop() {
	if a == nil || a.registry == nil {
		return
	}

	// Bound the drain so a wedged turn cannot hang shutdown indefinitely.
	ctx, cancel := context.WithTimeout(
		context.Background(), terminalNotifyTimeout,
	)
	defer cancel()

	a.stopWithDrain(ctx, a.registry.StopAndWait)
}

// stopWithDrain drains the registry goroutine via the supplied drain function,
// then stops the children only if the drain succeeded. It is factored out of
// Stop so the drain-timeout branch can be tested without a real durable actor.
//
// The happens-before stopChildren relies on holds ONLY when the drain returns
// nil: that is the path where process() has provably exited, so no concurrent
// ensureChild/dropChild can race stopChildren's unsynchronized iteration of
// r.active. On a drain timeout process() may still be running a wedged turn
// that mutates the map, so iterating it here would be a fatal concurrent map
// iteration and write. Skip stopChildren in that case: the children are durable
// actors registered with the actor system and are torn down by the system
// shutdown / process exit that follows.
func (a *OORRegistryActor) stopWithDrain(ctx context.Context,
	drain func(context.Context) error) {

	if err := drain(ctx); err != nil {
		a.behavior.log.WarnS(ctx, "Registry drain timed out; leaving "+
			"children to actor-system shutdown to avoid racing a "+
			"still-running turn", err)

		return
	}

	a.behavior.stopChildren()
}

// NewOORRegistryActor creates and starts the OOR registry coordinator. Call
// RestoreNonTerminal after construction to respawn in-flight sessions.
func NewOORRegistryActor(cfg OORRegistryConfig) (*OORRegistryActor, error) {
	// Validate the deps every spawned child needs up front so a
	// misconfiguration fails loudly at construction rather than
	// mid-transfer (after admission, possibly past the point of no return)
	// or, worse, silently disabling a safety net. Genuinely optional deps
	// -- SpendCompleter (nil => direct VTXO-store writes), LedgerSink,
	// TimeoutActor, CallbackRef, VTXOManager, and the observer -- are
	// intentionally not required here.
	if cfg.RegistryStore == nil {
		return nil, fmt.Errorf("registry store must be provided")
	}
	if cfg.DeliveryStore == nil {
		return nil, fmt.Errorf("delivery store must be provided")
	}
	if cfg.ServerConn == nil {
		return nil, fmt.Errorf("serverconn ref must be provided")
	}
	if cfg.Signer == nil {
		return nil, fmt.Errorf("signer must be provided")
	}
	if cfg.IncomingHandler == nil {
		return nil, fmt.Errorf("incoming handler must be provided")
	}
	if cfg.PackageStore == nil {
		return nil, fmt.Errorf("package store must be provided")
	}
	if cfg.ReservationStore == nil {
		return nil, fmt.Errorf("reservation store must be provided")
	}

	// SpendCompleter and VTXOStore are coupled: completeSpend filters
	// consumed inputs to locally-known outpoints via VTXOStore before
	// routing them to the SpendCompleter, so a SpendCompleter without a
	// VTXOStore would route unfiltered outpoints (including a
	// counterparty's) to the manager. Reject that combination here; in
	// production both are always wired.
	if cfg.SpendCompleter != nil && cfg.VTXOStore == nil {
		return nil, fmt.Errorf("vtxo store must be provided when a " +
			"spend completer is set")
	}

	// SpendReleaser is coupled to VTXOStore for the same reason as
	// SpendCompleter: releaseSpend filters to locally-known outpoints via
	// VTXOStore before routing them to the manager.
	if cfg.SpendReleaser != nil && cfg.VTXOStore == nil {
		return nil, fmt.Errorf("vtxo store must be provided when a " +
			"spend releaser is set")
	}

	// Normalize the receive limits once so the registry's own admission cap
	// (and the codec) enforce the package defaults for a partially-zeroed
	// config; otherwise a zero MaxConcurrentIncomingSessions would reject
	// every incoming admission.
	cfg.Limits = normalizeReceiveLimits(cfg.Limits)

	behavior := &oorRegistryBehavior{
		cfg:        cfg,
		log:        cfg.Log.UnwrapOr(btclog.Disabled),
		active:     make(map[SessionID]*OORSessionActor),
		activeDirs: make(map[SessionID]clientdb.OORSessionDirection),
		incoming:   make(map[SessionID]struct{}),
		parkedSelfHints: make(
			map[SessionID]*ResolveIncomingTransferRequest,
		),
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

	// A deferred self-transfer hint redelivers on a long flat backoff and
	// never dead-letters: the terminal-reap redrive is the fast path, so
	// the durable copy is purely the crash-safety net, and the default
	// exponential policy would both amplify write load while the writer
	// is saturated and dead-letter the hint after five attempts if the
	// outgoing session outlives the backoff schedule.
	durableCfg.TellRetryPolicy = func(err error, attempts int) (bool,
		time.Duration) {

		if errors.Is(err, errSelfTransferDeferred) {
			return true, selfHintRedeliveryBackoff
		}

		return actor.DefaultTellRetryPolicy(err, attempts)
	}

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

	// activeDirs tracks the direction of each resident child in active. A
	// terminal notification carries only the session id, so the registry
	// uses this map to distinguish a stale outgoing-child notification from
	// a replacement incoming child admitted under the same self-transfer
	// session id.
	activeDirs map[SessionID]clientdb.OORSessionDirection

	// incoming tracks the session ids of resident incoming receive children
	// so admission can bound their count. It is a subset of active,
	// maintained alongside it on the registry goroutine: ensureChild adds
	// an incoming session, dropChild removes any session unconditionally (a
	// non-incoming delete is a no-op). The cap defends against an operator
	// pinning unbounded children by streaming unanswered incoming hints.
	incoming map[SessionID]struct{}

	// selfRef is the registry's own tell-only reference, handed to each
	// child so it can report a terminal commit for reaping.
	selfRef actor.TellOnlyRef[OORDurableMsg]

	// spawnFunc creates one per-session child actor. Overridable in tests.
	spawnFunc func(SessionID, clientdb.OORSessionDirection) (
		*OORSessionActor, error)

	// parkedSelfHints holds incoming self-transfer hints deferred because
	// their session is still active as outgoing, keyed by session id so a
	// repeated hint overwrites rather than accumulates. Entries are
	// redriven (self-Tell) when the outgoing session's terminal
	// notification reaps the child, and dropped when an admission for the
	// session succeeds. The map is bounded by the number of live outgoing
	// sessions: parking requires a non-terminal outgoing row in the
	// durable store, so fabricated hints cannot grow it. Registry-
	// goroutine-owned like active and incoming.
	parkedSelfHints map[SessionID]*ResolveIncomingTransferRequest

	// pendingHandoff carries an outgoing admission's child future from
	// dispatch to Receive, so the caller's promise is detached and the
	// continuation wired only AFTER the consuming Commit succeeds. Wiring
	// it during dispatch (before the empty Commit) would leave an orphaned
	// continuation racing the framework's own promise completion when that
	// Commit fails (e.g. lease lost). It is set on the registry goroutine
	// in handleStartTransfer and consumed in Receive in the same turn.
	pendingHandoff *admissionHandoff

	// detachedWaitTimeout bounds the detached-continuation wait on a
	// child's response. Zero means detachedWaitTimeout (the package
	// default); tests shrink it to assert the goroutine exits without
	// leaking when a child future never resolves under an uncancellable
	// caller context.
	detachedWaitTimeout time.Duration
}

// detachWaitTimeout returns the configured detached-continuation wait bound,
// falling back to the package default when unset.
func (r *oorRegistryBehavior) detachWaitTimeout() time.Duration {
	if r.detachedWaitTimeout > 0 {
		return r.detachedWaitTimeout
	}

	return detachedWaitTimeout
}

// admissionHandoff records an outgoing admission whose caller promise must be
// detached onto the child's future after the registry's consuming Commit
// succeeds.
type admissionHandoff struct {
	sessionID SessionID
	child     *OORSessionActor
	future    actor.Future[ActorResp]
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

	// Trace every message landing at the registry so a routing decision can
	// be reconstructed from the log on the hot path.
	r.logger(ctx).TraceS(ctx, "Registry received message",
		btclog.Fmt("msg_type", "%T", msg),
		slog.Int("active_children", len(r.active)),
	)

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

	// Clear any handoff staged by a prior turn so a dispatch that stages a
	// new one is the only source consumed below.
	r.pendingHandoff = nil

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
		// The consuming Commit failed (e.g. lease lost), so the routing
		// message redelivers. Do NOT wire the caller-promise handoff:
		// the framework completes the caller with this error, and a
		// continuation wired here would race that completion and
		// outlive a turn whose forward will be replayed. The
		// redelivered turn (or boot resume) re-runs admission cleanly.
		r.pendingHandoff = nil

		return fn.Err[ActorResp](err)
	}

	// The routing turn committed: only now is it safe to detach the
	// caller's promise onto the child's admission future. Detaching before
	// this point would orphan the continuation if the Commit above failed.
	// With a detachable caller the child's result settles the promise
	// asynchronously and the routing turn returns its OK; without one (a
	// redelivered turn or a test) the admission is awaited inline and that
	// result becomes the turn's result.
	if r.pendingHandoff != nil {
		handoff := r.pendingHandoff
		r.pendingHandoff = nil

		inlineRes := r.completeAdmissionHandoff(ctx, handoff)
		if inlineRes.IsSome() {
			return inlineRes.UnwrapOr(res)
		}
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
			r.logger(ctx).InfoS(ctx, "Idempotency-key dedup hit; "+
				"returning existing OOR session",
				slog.String(
					"idempotency_key", req.IdempotencyKey,
				),
				slog.String(
					"session_id",
					SessionID(existing.SessionID).String(),
				),
			)

			return fn.Ok[ActorResp](&StartTransferResponse{
				SessionID: SessionID(existing.SessionID),
				Existing:  true,
			})

		case !errors.Is(err, clientdb.ErrOORSessionNotFound):
			return fn.Err[ActorResp](err)
		}

		r.logger(ctx).DebugS(ctx, "Idempotency-key dedup miss; "+
			"admitting fresh OOR session",
			slog.String("idempotency_key", req.IdempotencyKey),
		)
	}

	// Build the deterministic session to learn its id. This FSM is
	// discarded; the spawned child rebuilds the identical one. Stop it
	// immediately so its driveMachine goroutine does not linger for the
	// daemon's lifetime (one leak per outgoing admission otherwise).
	session, _, err := NewSessionWithIdempotencyKey(
		ctx, req.Policy, req.Inputs, req.Recipients, req.IdempotencyKey,
	)
	if err != nil {
		return fn.Err[ActorResp](err)
	}
	sessionID := session.ID
	session.FSM.Stop()

	// A resident child only answers Existing when a durable row backs it. A
	// failed admission on the production (detachable) path is reaped
	// asynchronously via a SessionTerminalNotification, so during the
	// window before that notification is processed the freshly spawned
	// child is still in r.active even though its admission committed no
	// row. Returning Existing for that phantom would wedge the transfer: a
	// follow-up DriveEvent restores nothing (GetSession is not-found) and
	// errors as an unknown session. Confirm the durable row before
	// deduping; on a missing row drop the phantom synchronously here on the
	// registry goroutine and fall through to a fresh admission.
	if child, ok := r.active[sessionID]; ok {
		_, err := r.cfg.RegistryStore.GetSession(
			ctx, chainHashOf(sessionID),
		)
		switch {
		case err == nil:
			r.
				logger(ctx).
				DebugS(
					ctx,
					"Outgoing OOR session already "+
						"resident; returning existing",
					slog.String(
						"session_id",
						sessionID.String(),
					),
				)

			return fn.Ok[ActorResp](&StartTransferResponse{
				SessionID: sessionID,
				Existing:  true,
			})

		case errors.Is(err, clientdb.ErrOORSessionNotFound):
			r.logger(ctx).DebugS(ctx, "Dropping row-less phantom "+
				"outgoing child before re-admitting",
				slog.String("session_id", sessionID.String()),
			)

			r.dropChild(sessionID, child)

		case err != nil:
			return fn.Err[ActorResp](err)
		}
	}

	child, err := r.ensureChild(
		sessionID, clientdb.OORSessionDirectionOutgoing,
	)
	if err != nil {
		return fn.Err[ActorResp](err)
	}

	r.logger(ctx).InfoS(ctx, "Admitting outgoing OOR session",
		slog.String("session_id", sessionID.String()),
		slog.Int("num_inputs", len(req.Inputs)),
		slog.Int("num_recipients", len(req.Recipients)),
	)

	// Forward the admission to the child: the Ask persists the request in
	// the child's durable mailbox so the forward survives a crash. The
	// caller-promise handoff (detach + continuation wiring, or the inline
	// await) is deferred to Receive AFTER the registry's consuming Commit
	// succeeds -- wiring it here, before that Commit, would orphan a
	// continuation racing the framework's own promise completion if the
	// Commit fails (e.g. lease lost) and the routing message redelivers.
	future := child.Ref().Ask(ctx, req)
	r.pendingHandoff = &admissionHandoff{
		sessionID: sessionID,
		child:     child,
		future:    future,
	}

	return fn.Ok[ActorResp](&StartTransferResponse{SessionID: sessionID})
}

// completeAdmissionHandoff settles the caller's promise for an outgoing
// admission after the registry's consuming Commit has succeeded. With a
// detachable caller it detaches the promise and wires the child's admission
// future so the registry goroutine never parks on the child's signing turn,
// returning fn.None (the routing turn keeps its own OK result); without one (a
// redelivered turn or a test driving the behavior directly) it awaits inline,
// dropping a phantom child on failure, and returns fn.Some(result) so the
// caller observes the admission outcome. It must run on the registry goroutine
// while the turn context is still live.
//
// wait and cleanup contexts are intentionally rooted in CallerCtx /
// context.Background, not the turn ctx, which is cancelled when the turn
// returns.
//
//nolint:contextcheck // the detached continuation outlives the turn ctx; its
func (r *oorRegistryBehavior) completeAdmissionHandoff(ctx context.Context,
	handoff *admissionHandoff) fn.Option[fn.Result[ActorResp]] {

	sessionID := handoff.sessionID
	child := handoff.child
	future := handoff.future

	detachedAsk, ok := actor.DetachAskPromise[ActorResp](ctx)
	if !ok {
		// No detachable caller (a redelivered turn, or a test driving
		// the behavior directly): preserve the synchronous semantics.
		r.logger(ctx).DebugS(ctx, "Awaiting outgoing admission inline "+
			"(no detachable caller)",
			slog.String("session_id", sessionID.String()),
		)

		res := future.Await(ctx)
		if res.IsErr() {
			// A failed admission turn commits no durable row, so
			// keeping the child registered would turn the in-memory
			// dedup into a phantom: a retry of the same transfer
			// would be told Existing for a session with no durable
			// backing. Drop the freshly spawned child so the retry
			// admits cleanly.
			r.dropChild(sessionID, child)
		}

		return fn.Some(res)
	}

	selfRef := r.selfRef

	r.logger(ctx).DebugS(ctx, "Detached admission promise to child; "+
		"registry turn complete",
		slog.String("session_id", sessionID.String()),
	)

	// Bound the continuation wait. On this durable path CallerCtx is the
	// registry actor's lifetime context, not the originating caller's (the
	// durable mailbox does not persist the caller's context with the Ask),
	// so it does not cancel on a caller hang-up; a wedged child turn would
	// otherwise leak this goroutine until the actor stops. This timeout is
	// therefore the only non-child unblock that always fires, so the
	// goroutine eventually exits even if the child never answers. The wait
	// is intentionally rooted in CallerCtx, not the turn ctx, which is
	// cancelled the instant this turn returns. The caller's own deadline is
	// observed only by its future.Await, never by this continuation.
	waitCtx, waitCancel := context.WithTimeout(
		detachedAsk.CallerCtx, r.detachWaitTimeout(),
	)

	future.OnComplete(
		waitCtx, func(res fn.Result[ActorResp]) {
			defer waitCancel()

			// OnComplete resolves the instant waitCtx is done, even
			// while the child is still signing its admission turn:
			// either the caller hung up or the detachedWaitTimeout
			// fired. Neither is a genuine admission failure -- the
			// child's turn keeps running under its own receive-loop
			// context, and reaping it here would stop a
			// legitimately admitting session out from under its
			// in-flight signing turn. Only reap when the error is a
			// real admission failure, i.e. the wait context is
			// still live.
			waitDone := waitCtx.Err() != nil

			// A failed admission leaves a fresh child with no
			// durable row. The continuation runs off the registry
			// goroutine, so route the cleanup through the
			// registry's own mailbox: the reaper treats a missing
			// row as reap-eligible and drops the phantom there. The
			// reaper re-checks the durable row, so a child that has
			// since committed a non-terminal row is left alone.
			if res.IsErr() && !waitDone && selfRef != nil {
				r.logger(detachedAsk.CallerCtx).WarnS(
					detachedAsk.CallerCtx,
					"Outgoing admission failed; reaping "+
						"phantom child",
					res.Err(),
					slog.String(
						"session_id",
						sessionID.String(),
					),
				)

				nctx, cancel := context.WithTimeout(
					context.Background(),
					terminalNotifyTimeout,
				)
				defer cancel()

				// The cleanup Tell is best effort: a missed
				// reap only leaves the phantom resident until
				// shutdown. It is enqueued before the caller's
				// promise completes (below), so a retry the
				// caller issues on the failure can only land
				// after this reap in the registry's ordered
				// mailbox: handleSessionTerminal drops this
				// phantom before any re-admission under the
				// deterministic session id exists.
				_ = selfRef.Tell(
					nctx, &SessionTerminalNotification{
						SessionID: sessionID,
					},
				)
			}

			detachedAsk.Promise.Complete(res)
		},
	)

	return fn.None[fn.Result[ActorResp]]()
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
		// lookupOrRestore returns a nil child for both a truly-unknown
		// session and a present-but-terminal one. A redelivered
		// duplicate server push (the operator mailbox is at-least-once)
		// for a session that has since reached a terminal snapshot and
		// been reaped is normal traffic: absorb it as an idempotent
		// no-op so it acks cleanly, rather than erroring into a
		// Nack/retry/dead-letter loop. Only a genuinely-unknown session
		// is an error.
		terminal, terr := r.sessionIsTerminal(ctx, req.SessionID)
		if terr != nil {
			return fn.Err[ActorResp](terr)
		}
		if terminal {
			r.logger(ctx).DebugS(ctx, "Dropping duplicate "+
				"drive-event for terminal session",
				slog.String(
					"session_id", req.SessionID.String(),
				),
				btclog.Fmt("event_type", "%T", req.Event),
			)

			return fn.Ok[ActorResp](&DriveEventResponse{})
		}

		return fn.Err[ActorResp](
			fmt.Errorf("unknown session: %s", req.SessionID),
		)
	}

	r.logger(ctx).TraceS(ctx, "Routing drive-event to session child",
		slog.String("session_id", req.SessionID.String()),
		btclog.Fmt("event_type", "%T", req.Event),
	)

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
		// Park a deferred self-transfer hint so the outgoing
		// session's terminal reap can redrive it immediately; the
		// erroring delivery stays in the durable mailbox on a long
		// backoff as the crash-safety fallback.
		if errors.Is(err, errSelfTransferDeferred) {
			r.parkSelfHint(req)
		}

		return fn.Err[ActorResp](err)
	}

	// Any parked copy of this hint is now superseded by the admission
	// below (a redriven or redelivered duplicate forwards idempotently to
	// the resident session).
	delete(r.parkedSelfHints, req.SessionID)

	_, existed := r.active[req.SessionID]

	// Bound the number of resident incoming children one daemon admits at
	// once. A new admission past the cap is rejected before any child is
	// spawned or any control-plane row is written, so an operator streaming
	// unanswered hints (each a distinct fabricated session id over an owned
	// receive script) cannot pin unbounded goroutines, mailboxes, and rows.
	// A resident session forwarding a follow-up hint is exempt: it already
	// counts against the cap. The hint is retried by transport, so an
	// over-cap rejection is recoverable once earlier sessions terminate and
	// are reaped.
	if !existed {
		maxIncoming := r.cfg.Limits.MaxConcurrentIncomingSessions
		if _, counted := r.incoming[req.SessionID]; !counted &&
			uint32(len(r.incoming)) >= maxIncoming {

			r.logger(ctx).WarnS(ctx, "Rejecting incoming OOR "+
				"admission at concurrency cap",
				errIncomingAdmissionCapped,
				slog.String(
					"session_id", req.SessionID.String(),
				),
				slog.Uint64(
					"active_incoming",
					uint64(
						len(r.incoming),
					),
				),
				slog.Uint64("cap", uint64(maxIncoming)),
			)

			return fn.Err[ActorResp](errIncomingAdmissionCapped)
		}
	}

	child, err := r.ensureChild(
		req.SessionID, clientdb.OORSessionDirectionIncoming,
	)
	if err != nil {
		return fn.Err[ActorResp](err)
	}

	if !existed {
		r.logger(ctx).InfoS(ctx, "Admitting incoming OOR session",
			slog.String("session_id", req.SessionID.String()),
			slog.Uint64(
				"recipient_event_id", req.RecipientEventID,
			),
		)
	} else {
		r.logger(ctx).DebugS(ctx, "Forwarding incoming hint to "+
			"resident session",
			slog.String("session_id", req.SessionID.String()),
		)
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

	// Deferring is an error so the durable delivery is retained (nacked on
	// the long self-transfer backoff); the caller parks the hint for the
	// event-driven redrive at the outgoing session's terminal reap.
	if !record.Status.IsTerminal() {
		r.logger(ctx).DebugS(ctx,
			"Deferring incoming self-transfer hint until "+
				"outgoing session reaches terminal state",
			slog.String("session_id", sessionID.String()),
			slog.String("phase", record.Phase),
		)

		return fmt.Errorf("%w: session %s", errSelfTransferDeferred,
			sessionID)
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

// parkSelfHint records a deferred self-transfer hint for the event-driven
// redrive at the outgoing session's terminal reap. Lazily initializes the map
// so directly constructed test behaviors stay valid.
func (r *oorRegistryBehavior) parkSelfHint(
	req *ResolveIncomingTransferRequest) {

	if r.parkedSelfHints == nil {
		r.parkedSelfHints = make(
			map[SessionID]*ResolveIncomingTransferRequest,
		)
	}

	r.parkedSelfHints[req.SessionID] = req
}

// redriveParkedSelfHint re-tells a parked self-transfer hint into the
// registry's own mailbox once its outgoing session has gone terminal. The
// hint's original durable delivery is still pending on the long self-transfer
// backoff, so a failed (or rolled-back) re-tell only costs latency, never the
// hint itself.
func (r *oorRegistryBehavior) redriveParkedSelfHint(ctx context.Context,
	sessionID SessionID) {

	req, ok := r.parkedSelfHints[sessionID]
	if !ok {
		return
	}
	delete(r.parkedSelfHints, sessionID)

	if r.selfRef == nil {
		return
	}

	if err := r.selfRef.Tell(ctx, req); err != nil {
		r.logger(ctx).WarnS(ctx, "Failed to redrive parked "+
			"self-transfer hint; durable redelivery will retry",
			err,
			slog.String("session_id", sessionID.String()),
		)

		return
	}

	r.logger(ctx).DebugS(ctx, "Redrove parked self-transfer hint",
		slog.String("session_id", sessionID.String()),
	)
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
		// The child may already be gone (duplicate notification or a
		// reap that raced the hint), but a parked self-transfer hint
		// still wants its event-driven wakeup. A premature redrive is
		// harmless: admission re-checks the durable row and re-parks
		// if the session is somehow still active.
		r.redriveParkedSelfHint(ctx, msg.SessionID)

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

	case r.activeDirectionKnown(msg.SessionID) &&
		record.Direction != r.activeDirs[msg.SessionID]:

		r.logger(ctx).DebugS(ctx, "Ignoring stale terminal OOR "+
			"session notification for replaced child",
			slog.String("session_id", msg.SessionID.String()),
			slog.Int("record_direction", int(record.Direction)),
			slog.Int(
				"active_direction",
				int(r.activeDirs[msg.SessionID]),
			),
		)

		return fn.Ok[ActorResp](&DriveEventResponse{})

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

	// The reap is the event-driven wakeup for a change-output hint that
	// arrived while this session was still active as outgoing.
	r.redriveParkedSelfHint(ctx, msg.SessionID)

	// Terminal rows are retained on disk (all directions, completed and
	// failed alike) so failed sessions stay visible to status RPCs and for
	// diagnostics; only the in-memory child is reaped here. A bounded
	// retention sweep, if ever needed, should age out all terminal rows
	// uniformly rather than deleting one class at reap time.
	return fn.Ok[ActorResp](&DriveEventResponse{})
}

// activeDirectionKnown reports whether the registry knows the direction of the
// resident child. Production children are installed through ensureChild, which
// records the direction; tests and hand-constructed behaviors may still install
// active entries directly, and those should keep the historical reap behavior.
func (r *oorRegistryBehavior) activeDirectionKnown(sessionID SessionID) bool {
	_, ok := r.activeDirs[sessionID]

	return ok
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

	r.logger(ctx).DebugS(ctx, "Routing retry resume to session child",
		slog.String("session_id", req.SessionID.String()),
	)

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
//
// wait context is intentionally rooted in CallerCtx, not the turn ctx, which is
// cancelled when the turn returns.
//
//nolint:contextcheck // the detached continuation outlives the turn ctx; its
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

	// Bound the continuation wait so a wedged child turn cannot leak this
	// goroutine: the caller may pass an uncancellable context (the read
	// probe shares the StartTransfer call site's context.WithoutCancel
	// derivation), so the detachedWaitTimeout cancellation is the only
	// non-child unblock that always fires. The wait is intentionally rooted
	// in CallerCtx, not the turn ctx, which is cancelled the instant this
	// turn returns.
	waitCtx, waitCancel := context.WithTimeout(
		detachedAsk.CallerCtx, r.detachWaitTimeout(),
	)

	future.OnComplete(
		waitCtx, func(res fn.Result[ActorResp]) {
			defer waitCancel()

			detachedAsk.Promise.Complete(res)
		},
	)

	return fn.Ok[ActorResp](&DriveEventResponse{})
}

// sessionIsTerminal reports whether the durable row for a session exists and
// has reached a terminal status. A missing row reports (false, nil): it is
// genuinely unknown, not terminal. Used by handleDriveEvent to distinguish a
// benign duplicate for a reaped terminal session from a truly-unknown one.
func (r *oorRegistryBehavior) sessionIsTerminal(ctx context.Context,
	sessionID SessionID) (bool, error) {

	record, err := r.cfg.RegistryStore.GetSession(
		ctx, chainHashOf(sessionID),
	)
	switch {
	case errors.Is(err, clientdb.ErrOORSessionNotFound):
		return false, nil

	case err != nil:
		return false, err
	}

	return record.Status.IsTerminal(), nil
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

	r.logger(ctx).DebugS(ctx, "Lazily respawning non-resident OOR "+
		"session child for routed message",
		slog.String("session_id", sessionID.String()),
		slog.String("phase", record.Phase),
	)

	return r.ensureChild(sessionID, record.Direction)
}

// ensureChild returns the active child for a session, spawning and registering
// it when absent. For an incoming session it enforces the concurrency cap as a
// last line of defense: every resident-making path (admission, lazy restore on
// a routed message, boot restore) funnels through here, so the bound holds even
// if a caller forgets the pre-spawn check in handleResolveIncoming.
func (r *oorRegistryBehavior) ensureChild(sessionID SessionID,
	direction clientdb.OORSessionDirection) (*OORSessionActor, error) {

	if child, ok := r.active[sessionID]; ok {
		return child, nil
	}

	if direction == clientdb.OORSessionDirectionIncoming {
		maxIncoming := r.cfg.Limits.MaxConcurrentIncomingSessions
		if _, counted := r.incoming[sessionID]; !counted &&
			uint32(len(r.incoming)) >= maxIncoming {
			return nil, errIncomingAdmissionCapped
		}
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
	r.activeDirs[sessionID] = direction
	if direction == clientdb.OORSessionDirectionIncoming {
		r.incoming[sessionID] = struct{}{}
	}

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
	delete(r.activeDirs, sessionID)
	delete(r.incoming, sessionID)
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
		SpendReleaser:        r.cfg.SpendReleaser,
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
// in-memory and do not survive a restart). Incoming restores are bounded by the
// concurrency cap so a corrupted backlog of more than the cap of non-terminal
// incoming rows cannot wedge the subsystem on every boot: rows are restored
// oldest-first (the oldest sessions are the closest to their resolve give-up),
// and over-cap incoming rows are skipped rather than aborting the whole
// restore. Outgoing sessions carry no concurrency cap and are always restored.
func (r *oorRegistryBehavior) restoreNonTerminal(ctx context.Context) error {
	rows, err := r.cfg.RegistryStore.ListNonTerminal(ctx)
	if err != nil {
		return err
	}

	// Restore oldest-first so a backlog larger than the cap admits the
	// oldest incoming sessions (nearest their give-up) and skips the
	// newest, which transport will redeliver once slots free up.
	sort.SliceStable(rows, func(i, j int) bool {
		return rows[i].CreatedAt.Before(rows[j].CreatedAt)
	})

	r.logger(ctx).InfoS(ctx, "Restoring non-terminal OOR sessions on boot",
		slog.Int("num_sessions", len(rows)),
	)

	var skippedIncoming int
	for i := range rows {
		sessionID := SessionID(rows[i].SessionID)
		if _, ok := r.active[sessionID]; ok {
			continue
		}

		child, err := r.ensureChild(sessionID, rows[i].Direction)

		// An incoming row over the concurrency cap is skipped, not
		// fatal: the durable row survives, and a slot freed by an
		// earlier session terminating lets a redelivered hint restore
		// it later. This keeps a corrupted over-cap backlog from
		// aborting every boot.
		if errors.Is(err, errIncomingAdmissionCapped) {
			skippedIncoming++

			continue
		}
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
			slog.Int("direction", int(rows[i].Direction)),
		)
	}

	if skippedIncoming > 0 {
		r.logger(ctx).WarnS(ctx, "Skipped over-cap incoming OOR "+
			"sessions during boot restore",
			errIncomingAdmissionCapped,
			slog.Int("skipped_incoming", skippedIncoming),
			slog.Uint64(
				"cap", uint64(
					r.cfg.Limits.MaxConcurrentIncomingSessions,
				),
			),
		)
	}

	return nil
}
