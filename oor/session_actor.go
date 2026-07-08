package oor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/batchcanon"
	"github.com/lightninglabs/wavelength/build"
	clientdb "github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/ledger"
	"github.com/lightninglabs/wavelength/rpc/oorpb"
	"github.com/lightninglabs/wavelength/serverconn"
	"github.com/lightninglabs/wavelength/timeout"
	"github.com/lightninglabs/wavelength/vtxo"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
)

// terminalNotifyTimeout bounds the advisory reap notification a child sends
// the registry after committing a terminal snapshot.
const terminalNotifyTimeout = 10 * time.Second

// SessionActorConfig configures one durable per-session OOR actor. The registry
// builds one of these per session via its childConfig, so every field except
// SessionID/Direction is shared across all sessions owned by the same daemon.
type SessionActorConfig struct {
	// SessionID is the session this actor owns.
	SessionID SessionID

	// Direction records whether the session is outgoing or incoming.
	Direction clientdb.OORSessionDirection

	// Log is an optional logger.
	Log fn.Option[btclog.Logger]

	// Signer signs Ark and checkpoint PSBTs inline during the turn.
	Signer input.Signer

	// IncomingHandler executes the incoming local-persistence outbox events
	// (metadata query filtering and VTXO materialization) inline. It is the
	// shared LocalPersistenceOutboxHandler, reused so the materialization
	// logic and its wallet resolvers are not reimplemented on this path.
	// Its writes join the turn transaction via the request context.
	IncomingHandler OutboxHandler

	// RegistryStore is the single source of truth for this session's
	// durable state. The actor writes its snapshot here inside Commit
	// (joining the turn transaction) and the registry reads it for restore
	// and listing.
	RegistryStore SessionRegistryStore

	// DeliveryStore backs the durable mailbox and the cross-actor outbox.
	DeliveryStore actor.DeliveryStore

	// ServerConn receives transport messages (submit, finalize, ack,
	// indexer queries) for durable delivery to the operator.
	ServerConn actor.TellOnlyRef[serverconn.ServerConnMsg]

	// VTXOManager receives notifications after incoming VTXOs are
	// materialized so it can spawn monitoring actors.
	VTXOManager actor.TellOnlyRef[vtxo.ManagerMsg]

	// BatchCanonicality, when set, receives a RegisterBatchRequest for
	// each commitment batch in a received OOR VTXO's lineage so the
	// reorg-safety availability gate (darepo#454) can govern the received
	// VTXO: a reorg-out or invalidation of any ancestor batch marks the
	// VTXO limbo. None disables registration (the gate stays dormant),
	// matching the C5/C6 dormancy contract.
	BatchCanonicality fn.Option[actor.TellOnlyRef[batchcanon.ManagerMsg]]

	// SpendCompleter routes outgoing input-spend completion through the
	// VTXO manager. The manager's status write commits in the VTXO actor's
	// own transaction, so it does NOT join this actor's turn: the spend is
	// run inline in dispatch (no OOR writer held) before the FSM advances
	// to Completed, and is re-driven idempotently on boot if the turn
	// crashes before committing. When nil, the package store writes spends
	// directly.
	SpendCompleter SpendCompleter

	// SpendReleaser routes input-reservation release through the VTXO
	// manager, returning each reserved VTXO from SpendingState to
	// LiveState. It fires from driveOutbox when a pre-point-of-no-return
	// terminal failure (e.g. a typed submit rejection) emits a
	// ReleaseInputsRequest, so a rejected submit does not strand
	// spendable funds until the startup sweep. Best-effort: the session
	// is already terminal Failed, so a release error only leaves the
	// sweep as the backstop. When nil, the inputs stay reserved.
	SpendReleaser SpendReleaser

	// ReservationStore records one durable spending-reservation row per
	// outgoing input. When nil, reservations are not recorded.
	ReservationStore ReservationStore

	// PackageStore persists finalized outgoing packages and bindings.
	PackageStore PackagePersistence

	// VTXOStore reloads materialized incoming VTXOs by outpoint.
	VTXOStore vtxo.VTXOStore

	// LedgerSink optionally receives ledger accounting messages. The sink
	// resolves the durable ledger actor, so a Tell issued inside this
	// session's commit transaction persists the message into the ledger's
	// durable mailbox atomically with the snapshot (DurableMailbox.Send
	// joins the ambient transaction): a committed turn can never lose its
	// accounting to a crash.
	LedgerSink fn.Option[ledger.Sink]

	// IncomingVTXOObserver receives incoming VTXO descriptors after
	// materialization.
	IncomingVTXOObserver IncomingVTXONotifier

	// Limits bounds incoming receive payloads.
	Limits ReceiveLimits

	// TimeoutActor schedules retry timers. When nil, retries are no-ops and
	// callers must resume explicitly.
	TimeoutActor actor.TellOnlyRef[timeout.Msg]

	// CallbackRef receives timeout expiry notifications mapped into
	// ResumeSessionRequest.
	CallbackRef actor.TellOnlyRef[*timeout.ExpiredMsg]

	// Registry receives a SessionTerminalNotification after this session
	// commits a terminal snapshot, so the coordinator can reap the child.
	// When nil, terminal sessions stay resident until shutdown.
	Registry actor.TellOnlyRef[OORDurableMsg]
}

// OORSessionActor wraps one durable per-session OOR actor.
type OORSessionActor struct {
	ref     actor.ActorRef[OORDurableMsg, ActorResp]
	tellRef actor.TellOnlyRef[OORDurableMsg]
	durable *actor.DurableActor[OORDurableMsg, ActorResp]
	stop    func()
}

// Ref returns the public actor reference.
func (a *OORSessionActor) Ref() actor.ActorRef[OORDurableMsg, ActorResp] {
	return a.ref
}

// TellRef returns a tell-only reference.
func (a *OORSessionActor) TellRef() actor.TellOnlyRef[OORDurableMsg] {
	return a.tellRef
}

// Stop stops the underlying durable actor.
func (a *OORSessionActor) Stop() {
	if a == nil {
		return
	}

	if a.stop != nil {
		a.stop()

		return
	}

	if a.durable != nil {
		a.durable.Stop()
	}
}

// NewOORSessionActor creates and starts one durable per-session OOR actor,
// restoring its FSM from the registry row before the mailbox starts draining.
func NewOORSessionActor(cfg SessionActorConfig) (*OORSessionActor, error) {
	if cfg.RegistryStore == nil {
		return nil, fmt.Errorf("registry store must be provided")
	}
	if cfg.DeliveryStore == nil {
		return nil, fmt.Errorf("delivery store must be provided")
	}

	// Normalize the receive limits once so every consumer -- the codec,
	// the in-memory metadata query, and the restore decode -- enforces the
	// same caps for a partially-zeroed config.
	cfg.Limits = normalizeReceiveLimits(cfg.Limits)

	actorID := ActorIDForSession(cfg.SessionID)

	behavior := &sessionBehavior{
		cfg:       cfg,
		actorID:   actorID,
		sessionID: cfg.SessionID,
		direction: cfg.Direction,
		log:       cfg.Log.UnwrapOr(btclog.Disabled),
	}
	if err := behavior.restore(context.Background()); err != nil {
		return nil, err
	}

	durableCfg := actor.DefaultDurableTxActorConfig[
		OORDurableMsg, ActorResp, oorTx,
	](
		actorID, behavior, behavior.bindStores, cfg.DeliveryStore,
		newOORActorCodec(cfg.Limits),
	)
	durableCfg.Log = cfg.Log

	durable, err := actor.NewDurableActor(durableCfg).Unpack()
	if err != nil {
		return nil, err
	}
	behavior.selfRef = durable.TellRef()
	durable.Start()

	return &OORSessionActor{
		ref:     durable.Ref(),
		tellRef: durable.TellRef(),
		durable: durable,
		// Stop the durable actor first so the mailbox loop drains and
		// no turn can touch the FSM, then stop the FSM goroutine
		// itself: it is started on a non-cancelling context, so
		// durable.Stop alone would leak its driveMachine goroutine.
		stop: func() {
			durable.Stop()
			behavior.stopFSM()
		},
	}, nil
}

// oorTx is the transaction-scoped store handed to the session behavior inside
// each Read/Stage/Commit phase. It carries the per-call DeliveryStore (whose
// EnqueueOutbox joins the framework transaction via the closure context) plus
// the actor id. The registry snapshot write joins the same transaction via the
// behavior's RegistryStore, which reads the *sql.Tx off the closure context.
type oorTx struct {
	store   actor.DeliveryStore
	actorID string
}

// bindStores is the StoreFactory for the session Read/Stage/Commit path.
func (b *sessionBehavior) bindStores(_ context.Context,
	ds actor.DeliveryStore) oorTx {

	return oorTx{
		store:   ds,
		actorID: b.actorID,
	}
}

// setFSM installs a new session FSM, stopping any previously running one first.
// The FSM is started on a non-cancelling context so it survives across turns
// (see the reload path), which means it is NOT reaped when its turn context is
// cancelled. Without an explicit Stop on every replacement, a reloaded FSM's
// driveMachine goroutine would linger for the daemon's lifetime.
func (b *sessionBehavior) setFSM(fsm *StateMachine) {
	if b.fsm != nil && b.fsm != fsm {
		b.fsm.Stop()
	}
	b.fsm = fsm
}

// stopFSM stops the running session FSM, if any, so its driveMachine goroutine
// exits. Called on actor teardown (the FSM is otherwise tied to a
// non-cancelling context) and before a reload rebuilds it.
func (b *sessionBehavior) stopFSM() {
	if b.fsm != nil {
		b.fsm.Stop()
		b.fsm = nil
	}
}

// sessionBehavior runs on the durable actor Read/Stage/Commit execution path
// for a single OOR session. The FSM emits outbox events as before, but this
// behavior handles them itself in one shared switch (driveOutbox) right after
// driving the FSM -- no separate OutboxHandler indirection -- so the control
// flow reads straight from the code: sign inline, enqueue cross-actor transport
// to serverconn, notify the VTXO manager, schedule retries. Local signing runs
// in dispatch with no writer held; commitAck persists the registry snapshot and
// folds the lease-fenced ack in one short transaction.
type sessionBehavior struct {
	cfg     SessionActorConfig
	actorID string
	log     btclog.Logger
	selfRef actor.TellOnlyRef[OORDurableMsg]

	sessionID SessionID
	direction clientdb.OORSessionDirection

	// fsm is the running session state machine, lazily restored.
	fsm *StateMachine

	// loaded reports whether the FSM has been restored/created.
	loaded bool

	// pendingTransport accumulates cross-actor transport messages produced
	// while draining the FSM in dispatch; commitAck enqueues them durably
	// in the same transaction as the snapshot and the ack.
	pendingTransport []serverconn.ServerConnMsg

	// pendingLedger accumulates accounting messages produced while
	// draining the FSM in dispatch; commitAck Tells them to the durable
	// ledger actor inside the same transaction as the snapshot, so a
	// committed turn can never lose its ledger entries to a crash. The
	// ledger actor's idempotent inserts absorb the at-least-once
	// redelivery of a replayed turn.
	pendingLedger []ledger.LedgerMsg

	// commitWork accumulates extra durable writes (reservations, the
	// finalized package, input-spend completion) that handlers stage during
	// dispatch; commitAck runs them inside the same writer transaction as
	// the snapshot and the ack so the turn is atomic.
	commitWork []func(ctx context.Context, tx oorTx) error

	// postCommit accumulates best-effort cross-actor notifications (VTXO
	// materialization, fraud-watch arming) fired after the turn commits.
	// Both targets re-derive their state from the persisted VTXO rows at
	// boot, so a crash inside this window loses only the in-process
	// notification, never the underlying work.
	postCommit []func(ctx context.Context)

	// pendingRetries accumulates retry timers requested while draining the
	// FSM; they are armed only after the turn commits, so a rolled-back
	// turn never schedules a timer and the timeout-actor Tell never runs
	// inside the commit closure.
	pendingRetries []*ScheduleRetryRequest

	// terminalCommitted reports whether the turn's committed snapshot
	// reached a terminal status, set by commitAck and consumed by the
	// registry reap notification after the turn.
	terminalCommitted bool

	// dedupKeyWinner is the session id of the surviving keyed session when
	// this child's commit lost an idempotency-key race: a concurrent
	// same-key admission selected different inputs (hence a different
	// session id) and committed its row first. commitAck sets it after the
	// in-commit lookup so Receive answers the caller StartTransferResponse{
	// Existing: true} for the surviving session instead of failing the turn
	// with a raw unique-constraint error. Set per turn; consumed by
	// Receive.
	dedupKeyWinner *SessionID

	// commitFailed reports that the previous turn advanced b.fsm in memory
	// but its Commit rolled back (a non-lease-loss error), leaving the
	// in-memory FSM observably ahead of the last durably-committed
	// snapshot. When set, the next driving turn re-runs restore() before
	// dispatch so the redelivered event re-applies against the
	// last-committed state instead of being discarded as already-applied
	// against an uncommitted advance. A lease-loss commit failure does not
	// set this: another instance fenced the turn and owns the state going
	// forward.
	commitFailed bool
}

// Compile-time check that sessionBehavior runs on the Read/Stage/Commit path.
var _ actor.TxBehavior[
	OORDurableMsg, ActorResp, oorTx,
] = (*sessionBehavior)(nil)

// logger returns the behavior logger bound to ctx.
func (b *sessionBehavior) logger(ctx context.Context) btclog.Logger {
	if b.log != btclog.Disabled {
		return b.log
	}

	return build.LoggerFromContext(ctx)
}

// Receive is the single entry point for every message that drives this
// session's FSM. Read-only probes return directly; everything else drains the
// FSM (with inline signing and cross-actor message collection) in dispatch and
// is consumed by one lease-fenced commitAck.
func (b *sessionBehavior) Receive(ctx context.Context, msg OORDurableMsg,
	ax actor.Exec[oorTx]) fn.Result[ActorResp] {

	switch msg.(type) {
	case *actor.RestartMessage:
		// Restore already ran at construction; the restart message
		// carries no state to persist, so the framework consumes it via
		// the non-transactional ack path.
		return fn.Ok[ActorResp](&DriveEventResponse{})

	case *GetStateRequest:
		return b.handleGetState()
	}

	// If the previous turn's Commit rolled back after advancing b.fsm in
	// memory, the in-memory FSM is ahead of the last durably-committed
	// snapshot. Rebuild it from the registry row before dispatch so the
	// redelivered driving event re-applies the full transition (re-staging
	// materialization and transport) against the last-committed state,
	// rather than being no-op'd by an FSM that already advanced on an
	// uncommitted turn. restore() rebuilds b.fsm from the durable row; on
	// success the flag is cleared and the redelivered event drives the
	// rebuilt FSM.
	if b.commitFailed {
		b.
			logger(ctx).
			WarnS(
				ctx,
				"Reloading session FSM after a failed "+
					"commit before redelivered turn",
				fmt.Errorf("commit rolled back"),
				slog.String("session_id", b.sessionID.String()),
			)

		// Drop the stale in-memory FSM so restore rebuilds it from the
		// last-committed row. A fresh session whose first commit never
		// landed has no row, leaving the behavior unloaded so its
		// admission event re-creates the FSM from scratch.
		//
		// restore() starts a fresh FSM goroutine bound to the context
		// it is given. An Ask-delivered turn runs under a context that
		// is cancelled the instant the turn returns, which would tear
		// down the rebuilt FSM before the next turn could drive it.
		// Strip the cancellation so the reloaded FSM survives past this
		// turn, matching the constructor's restore(context.Background).
		// stopFSM reaps the stale FSM's goroutine before we rebuild;
		// the rebuilt one is reaped on teardown via stopFSM.
		b.stopFSM()
		b.loaded = false
		if err := b.restore(context.WithoutCancel(ctx)); err != nil {
			b.
				logger(ctx).
				WarnS(
					ctx,
					"Failed to reload session FSM "+
						"after commit failure",
					err,
					slog.String(
						"session_id",
						b.sessionID.String(),
					),
				)

			return fn.Err[ActorResp](err)
		}
		b.commitFailed = false
	}

	// Reset the per-turn accumulators and drain the FSM. dispatch performs
	// every inline side effect, collects cross-actor messages, and stages
	// extra commit work, but never commits.
	b.pendingTransport = b.pendingTransport[:0]
	b.pendingLedger = b.pendingLedger[:0]
	b.commitWork = b.commitWork[:0]
	b.postCommit = b.postCommit[:0]
	b.pendingRetries = b.pendingRetries[:0]
	b.terminalCommitted = false
	b.dedupKeyWinner = nil

	// Trace the turn entry so a session's progress can be reconstructed
	// message by message from the log.
	b.logger(ctx).TraceS(ctx, "Session turn dispatch",
		slog.String("session_id", b.sessionID.String()),
		btclog.Fmt("msg_type", "%T", msg),
	)

	res := b.dispatch(ctx, msg)
	if res.IsErr() {
		// A lost lease is a benign concurrency signal (another instance
		// fenced this turn), not an internal bug; everything else is a
		// genuine dispatch failure worth surfacing.
		if errors.Is(res.Err(), actor.ErrLeaseLost) {
			b.logger(ctx).WarnS(ctx, "Session turn lost lease "+
				"during dispatch", res.Err(),
				slog.String(
					"session_id", b.sessionID.String(),
				),
			)
		} else {
			b.logger(ctx).WarnS(ctx, "Session turn dispatch failed",
				res.Err(),
				slog.String(
					"session_id", b.sessionID.String(),
				),
				btclog.Fmt("msg_type", "%T", msg),
			)

			// dispatch drives b.fsm in memory before its inline
			// side effects run (e.g. a FinalizeAcceptedEvent
			// advances past AwaitingFinalizeAccepted before
			// completeSpend executes). protofsm does not roll that
			// advance back on error, so an inline-effect failure
			// leaves the in-memory FSM ahead of the last-committed
			// snapshot exactly like a rolled-back Commit does. Mark
			// the FSM dirty so the redelivered turn reloads from
			// the durable row and re-applies the full transition --
			// otherwise the advanced-but-uncommitted FSM would
			// no-op the redelivered event, and the
			// snapshot/package/ ledger work gated on that
			// transition (e.g. captureFinalizeState) would be
			// silently skipped. A lease-loss error is handled above
			// and never sets this: the fencing instance owns the
			// state.
			b.commitFailed = true
		}

		return res
	}

	// Single consume point: persist the registry snapshot, run the staged
	// commit work, enqueue the collected cross-actor transport messages,
	// and fold the lease-fenced ack and dedup mark into one short writer
	// transaction. A lost lease surfaces here as actor.ErrLeaseLost.
	if err := b.commitAck(ctx, ax); err != nil {
		// A lost lease means a concurrent instance fenced this commit;
		// it is a normal lease-handoff outcome, not an internal bug.
		if errors.Is(err, actor.ErrLeaseLost) {
			b.logger(ctx).WarnS(ctx, "Session commit lost lease",
				err,
				slog.String(
					"session_id", b.sessionID.String(),
				),
			)
		} else {
			b.logger(ctx).WarnS(ctx, "Session commit failed", err,
				slog.String(
					"session_id", b.sessionID.String(),
				),
			)

			// The Commit rolled back but dispatch already advanced
			// b.fsm in memory (and, for an incoming receive, ran
			// the staged materialization that drove the FSM to its
			// terminal state inside the closure). Mark the FSM
			// dirty so the next redelivered turn reloads it from
			// the last-committed row before dispatch; otherwise the
			// advanced-but-uncommitted FSM would no-op the
			// redelivered driving event and a later turn would
			// persist a terminal snapshot whose effects (e.g.
			// materialized VTXOs) never committed. A lease-loss
			// failure is handled above and never sets this: the
			// fencing instance owns the state.
			b.commitFailed = true
		}

		return fn.Err[ActorResp](err)
	}

	// The turn consumed cleanly but lost an idempotency-key race: a
	// concurrent same-key admission committed the surviving session first.
	// Answer the caller with that session as Existing instead of the raw
	// unique-constraint failure. No transport, ledger, retry, or snapshot
	// work ran for this no-op turn (commitAck returned early before writing
	// a row).
	if b.dedupKeyWinner != nil {
		winner := *b.dedupKeyWinner

		b.logger(ctx).InfoS(ctx, "Outgoing admission deduped to "+
			"surviving keyed session",
			slog.String("session_id", b.sessionID.String()),
			slog.String("winner_session_id", winner.String()),
		)

		// This child wrote no durable row, so it is an orphan: a live
		// actor goroutine, mailbox, and receptionist key for a session
		// id with no backing row. Tell the registry to reap it. The
		// reaper re-checks the durable row and treats a no-row session
		// as reap-eligible, so routing the dedup loser through the
		// terminal notification drops it cleanly instead of leaving it
		// resident until shutdown.
		//
		//nolint:contextcheck // reap notification is daemon-owned, not
		// turn-scoped
		b.notifyTerminal()

		return fn.Ok[ActorResp](&StartTransferResponse{
			SessionID: winner,
			Existing:  true,
		})
	}

	// The committed turn is durable: record what it persisted at debug
	// granularity so a snapshot of every committed transition survives in
	// the log.
	b.logger(ctx).DebugS(ctx, "Session turn committed",
		slog.String("session_id", b.sessionID.String()),
		slog.Int("num_transport", len(b.pendingTransport)),
		slog.Int("num_ledger", len(b.pendingLedger)),
		slog.Int("num_retries", len(b.pendingRetries)),
		slog.Bool("terminal", b.terminalCommitted),
	)

	// Best-effort cross-actor notifications run only after the turn is
	// durably committed.
	for _, fn := range b.postCommit {
		fn(ctx)
	}

	// Arm the retry timers requested by this turn now that it committed.
	for _, retry := range b.pendingRetries {
		b.scheduleRetry(ctx, retry)
	}

	// A terminal commit retires this session: tell the registry so it can
	// stop this child and drop it from the active set.
	//
	//nolint:contextcheck // reap notification is daemon-owned, not
	// turn-scoped
	if b.terminalCommitted {
		b.logger(ctx).InfoS(ctx, "OOR session reached terminal state",
			slog.String("session_id", b.sessionID.String()),
			slog.Int("direction", int(b.direction)),
		)

		b.notifyTerminal()
	}

	return res
}

// notifyTerminal tells the registry this session committed a terminal
// snapshot. The Tell runs on its own goroutine with a daemon-owned context so
// a full registry mailbox can never wedge this actor's turn: the registry may
// be blocked on an admission Ask into this very actor, and the notification is
// advisory (a missed reap only leaves the child resident until shutdown, and
// the registry re-checks the durable row before reaping).
func (b *sessionBehavior) notifyTerminal() {
	if b.cfg.Registry == nil {
		return
	}

	registry := b.cfg.Registry
	sessionID := b.sessionID
	log := b.log

	go func() {
		ctx, cancel := context.WithTimeout(
			context.Background(), terminalNotifyTimeout,
		)
		defer cancel()

		err := registry.Tell(ctx, &SessionTerminalNotification{
			SessionID: sessionID,
		})
		if err != nil {
			log.WarnS(ctx, "Failed to notify registry of "+
				"terminal session", err,
				slog.String(
					"session_id", sessionID.String(),
				),
			)
		}
	}()
}

// dispatch maps one durable message onto the FSM event surface and drains the
// resulting outbox via driveOutbox. It performs inline signing and accumulates
// cross-actor transport but never commits.
func (b *sessionBehavior) dispatch(ctx context.Context,
	msg OORDurableMsg) fn.Result[ActorResp] {

	switch m := msg.(type) {
	case *StartTransferRequest:
		return b.handleStartTransfer(ctx, m)

	case *DriveEventRequest:
		return b.handleDriveEvent(ctx, m)

	case *ResolveIncomingTransferRequest:
		return b.handleResolveIncomingTransfer(ctx, m)

	case *ResumeSessionRequest:
		return b.handleResumeSession(ctx, m)

	default:
		return fn.Err[ActorResp](
			fmt.Errorf("unknown oor session message: %T", msg),
		)
	}
}

// apply drives one event into the FSM and returns the emitted outbox events.
func (b *sessionBehavior) apply(ctx context.Context, event Event) (
	[]OutboxEvent, error) {

	if b.fsm == nil {
		return nil, fmt.Errorf("session fsm not loaded")
	}

	fut := b.fsm.AskEvent(ctx, event)
	result := fut.Await(ctx)
	if result.IsErr() {
		return nil, result.Err()
	}

	return result.UnwrapOr(nil), nil
}

// driveOutbox runs the FSM's emitted outbox effects and classifies any failure:
// a deterministic *terminalOutboxError (operator data or persisted state that
// will fail identically on every redelivery) is converted into a terminal
// FailEvent so the turn commits and acks the message instead of rolling back
// into a doomed retry loop that ends in dead-letter; any other (transient)
// error is returned so the framework redelivers and the effect can succeed
// later.
//
// This is the top-level entry for a dispatch turn. continueWith and the
// commit-path materialization use the raw driveOutboxEvents directly so a
// deterministic error propagates up to this single classification point (and a
// commit-path failure rolls the transaction back rather than failing the FSM
// mid-commit).
func (b *sessionBehavior) driveOutbox(ctx context.Context,
	outbox []OutboxEvent) error {

	err := b.driveOutboxEvents(ctx, outbox)
	if err == nil {
		return nil
	}

	var terminal *terminalOutboxError
	if errors.As(err, &terminal) {
		b.
			logger(ctx).
			WarnS(
				ctx,
				"Failing OOR session on deterministic "+
					"outbox error",
				err,
				slog.String("session_id", b.sessionID.String()),
			)

		return b.failTerminal(ctx, terminal)
	}

	return err
}

// failTerminal drives the session FSM to its terminal Failed state for a
// deterministic outbox error, so the current turn commits a Failed snapshot and
// acks the message. The Failed transition emits no further outbox, so this does
// not recurse, and every non-terminal state that emits a transport effect
// handles FailEvent, so it always advances. A genuine apply error (should not
// happen) propagates and rolls the turn back.
func (b *sessionBehavior) failTerminal(ctx context.Context, cause error) error {
	return b.continueWith(ctx, &FailEvent{Reason: cause.Error()})
}

// driveOutboxEvents is the shared switch that handles every outbox event the
// FSM emits. This is the heart of the liberalized control flow: instead of
// routing events through a separate OutboxHandler, the actor executes each
// effect directly here and feeds any follow-up event straight back into the
// FSM. Local effects (signing) run inline with no writer held; cross-actor
// transport is collected into pendingTransport and durably enqueued by
// commitAck. It returns errors raw -- a deterministic effect wraps a
// *terminalOutboxError -- and the caller (driveOutbox) decides whether to fail
// the session or retry.
func (b *sessionBehavior) driveOutboxEvents(ctx context.Context,
	outbox []OutboxEvent) error {

	for _, event := range outbox {
		// Trace every outbox effect as it is dispatched: this is the
		// high-volume per-event detail that ties an FSM transition to
		// the side effect the actor ran for it.
		b.logger(ctx).TraceS(ctx, "Driving outbox event",
			slog.String("session_id", b.sessionID.String()),
			btclog.Fmt("event_type", "%T", event),
		)

		switch m := event.(type) {
		// Signing: sign inline using the wallet signer, then feed the
		// resulting signed event straight back into the FSM.
		case *RequestArkSignatures:
			if b.cfg.Signer == nil {
				return fmt.Errorf("signer is required")
			}

			b.logger(ctx).DebugS(ctx, "Signing Ark PSBT inline",
				slog.String(
					"session_id", b.sessionID.String(),
				),
				slog.Int(
					"num_checkpoints",
					len(m.CheckpointPSBTs),
				),
				slog.Int(
					"num_inputs", len(m.TransferInputs),
				),
			)

			err := SignArkPSBT(
				b.cfg.Signer, m.ArkPSBT, m.CheckpointPSBTs,
				m.TransferInputs,
			)
			if err != nil {
				return err
			}

			if err := b.continueWith(ctx, &ArkSignedEvent{
				ArkPSBT: m.ArkPSBT,
			}); err != nil {
				return err
			}

		case *RequestCheckpointSignatures:
			if b.cfg.Signer == nil {
				return fmt.Errorf("signer is required")
			}

			b.logger(ctx).DebugS(ctx,
				"Signing checkpoint PSBTs inline",
				slog.String(
					"session_id", b.sessionID.String(),
				),
				slog.Int(
					"num_checkpoints",
					len(m.CoSignedCheckpointPSBTs),
				),
			)

			err := SignCheckpointPSBTs(
				b.cfg.Signer, m.TransferInputs,
				m.CoSignedCheckpointPSBTs,
			)
			if err != nil {
				return err
			}

			if err := b.continueWith(ctx, &CheckpointsSignedEvent{
				FinalCheckpointPSBTs: m.CoSignedCheckpointPSBTs,
			}); err != nil {
				return err
			}

		// Transport: collect the cross-actor serverconn message. The
		// FSM stays in its Awaiting state; the operator's response
		// arrives as a fresh DriveEventRequest turn. commitAck enqueues
		// these.
		case *SendSubmitPackageRequest, *SendFinalizePackageRequest,
			*SendIncomingAckRequest, *QueryIncomingTransferRequest,
			*QueryIncomingMetadataRequest:

			transport, err := b.buildTransportMessage(ctx, event)
			if err != nil {
				return err
			}
			b.pendingTransport = append(
				b.pendingTransport, transport,
			)

			b.logger(ctx).DebugS(ctx, "Staged outbound transport "+
				"for commit enqueue",
				slog.String(
					"session_id", b.sessionID.String(),
				),
				btclog.Fmt("transport_type", "%T", event),
			)

			// SendIncomingAckRequest has no operator response;
			// advance the receive FSM to completion in the same
			// turn.
			if _, ok := m.(*SendIncomingAckRequest); ok {
				if err := b.continueWith(
					ctx, &IncomingAckSentEvent{},
				); err != nil {
					return err
				}
			}

		// Input-spend completion: run the spend inline in dispatch,
		// with no OOR writer transaction held, then advance the FSM to
		// completion only after the spend lands. completeSpend routes
		// through the VTXO manager, whose status write commits in the
		// VTXO actor's own transaction; that cross-actor write does NOT
		// join this turn's tx, so it must not run inside commitAck's
		// held writer transaction (a second writer awaited under the
		// single SQLite/Postgres writer lock would deadlock until
		// busy_timeout). The completion is therefore non-atomic with
		// the OOR snapshot: the FSM stays in AwaitingLocalVTXOUpdate
		// until this turn commits Completed, so a crash after the VTXO
		// is durably Spent but before that commit re-emits
		// MarkInputsSpentRequest on boot
		// (resumeOutboxForOutgoingState), and the manager's
		// isPersistedSpent check makes the replay an idempotent no-op.
		case *MarkInputsSpentRequest:
			outpoints := m.Outpoints
			b.logger(ctx).DebugS(ctx, "Completing input spend "+
				"inline",
				slog.String(
					"session_id", b.sessionID.String(),
				),
				slog.Int("num_outpoints", len(outpoints)),
			)
			if err := b.completeSpend(ctx, outpoints); err != nil {
				return err
			}

			if err := b.continueWith(
				ctx, &InputsMarkedSpentEvent{},
			); err != nil {
				return err
			}

		// Input-reservation release: a pre-point-of-no-return terminal
		// failure (e.g. a typed submit rejection) returns the reserved
		// inputs to LiveState. This is BEST-EFFORT and must never fail
		// the turn: the transition that emitted it already moved the
		// FSM to terminal Failed, so returning an error here would
		// re-drive that doomed transition and wedge the session in a
		// redelivery loop. A failed release just leaves the startup
		// sweep as the backstop it already is.
		case *ReleaseInputsRequest:
			if err := b.releaseSpend(ctx, m.Outpoints); err != nil {
				b.logger(ctx).WarnS(ctx, "Failed to release "+
					"reserved inputs after a "+
					"terminal session failure; "+
					"startup sweep will reconcile",
					err,
					slog.String(
						"session_id",
						b.sessionID.String(),
					),
					slog.Int(
						"num_outpoints",
						len(m.Outpoints),
					),
				)
			}

		// Incoming materialization: a DB write plus the follow-up FSM
		// advance, so it is staged into the commit transaction. The
		// materialize closure drives the IncomingHandledEvent it
		// produces (queueing the post-commit VTXO notification and
		// advancing toward the ack), which is why commitAck takes its
		// snapshot after the commit work runs.
		case *MaterializeIncomingVTXOsRequest:
			materialize := m
			b.logger(ctx).DebugS(ctx, "Staging incoming VTXO "+
				"materialization for commit",
				slog.String(
					"session_id", b.sessionID.String(),
				),
				slog.Int(
					"num_recipients",
					len(materialize.Recipients),
				),
			)
			b.commitWork = append(b.commitWork,
				func(txCtx context.Context, _ oorTx) error {
					return b.materializeIncoming(
						txCtx, materialize,
					)
				},
			)

		// Retry scheduling: validated here, armed after the commit.
		case *ScheduleRetryRequest:
			if err := b.queueRetry(m); err != nil {
				return err
			}

		// Informational notification: log only.
		case *IncomingTransferNotification:
			b.logger(ctx).InfoS(
				ctx,
				"Incoming transfer notification",
				slog.String("session_id", b.sessionID.String()),
				slog.Int("num_recipients", len(m.Recipients)),
			)

		default:
			return fmt.Errorf("unhandled outbox event %T", event)
		}
	}

	return nil
}

// continueWith drives a follow-up event into the FSM and recursively handles
// the outbox it emits. It uses the raw driveOutboxEvents so a deterministic
// error propagates up to the single top-level driveOutbox classifier rather
// than failing the session from inside the recursion.
func (b *sessionBehavior) continueWith(ctx context.Context, event Event) error {
	next, err := b.apply(ctx, event)
	if err != nil {
		return err
	}

	return b.driveOutboxEvents(ctx, next)
}

// queueRetry validates the retry wiring and defers the timer arm to after the
// turn commits. Validation runs here so a miswired daemon still fails the
// turn, while the side-effecting Tell never runs inside the commit closure.
func (b *sessionBehavior) queueRetry(msg *ScheduleRetryRequest) error {
	if b.cfg.TimeoutActor == nil {
		return nil
	}
	if b.cfg.CallbackRef == nil {
		return fmt.Errorf("timeout callback ref not wired")
	}

	b.pendingRetries = append(b.pendingRetries, msg)

	return nil
}

// scheduleRetry arms one retry timer via the timeout actor after the turn
// committed. The session id is the timeout id so the expiry callback can
// rebuild a ResumeSessionRequest. Failures are logged: the timer is an
// in-memory accelerant, and a lost one is re-derived by the boot-time resume.
func (b *sessionBehavior) scheduleRetry(ctx context.Context,
	msg *ScheduleRetryRequest) {

	b.logger(ctx).InfoS(ctx, "Scheduling retry",
		slog.String("session_id", b.sessionID.String()),
		slog.String("reason", msg.Reason),
		slog.Duration("after", msg.After),
	)

	err := b.cfg.TimeoutActor.Tell(ctx, &timeout.ScheduleTimeoutRequest{
		ID:       timeout.ID(b.sessionID.String()),
		Duration: msg.After,
		Callback: b.cfg.CallbackRef,
	})
	if err != nil {
		b.logger(ctx).WarnS(ctx, "Failed to schedule retry timer",
			err,
			slog.String("session_id", b.sessionID.String()),
		)
	}
}

// commitAck persists the current registry snapshot, enqueues any collected
// cross-actor transport, and folds the lease-fenced ack and dedup mark into one
// short writer transaction so the session's state advance and the message's
// consumption are exactly-once.
func (b *sessionBehavior) commitAck(ctx context.Context,
	ax actor.Exec[oorTx]) error {

	return ax.Commit(ctx, func(txCtx context.Context, tx oorTx) error {
		// Resolve an idempotency-key race BEFORE running any commit
		// work so a deduped loser writes nothing (no reservations, no
		// snapshot, no transport). The session id is the Ark txid,
		// derived from the selected inputs, so two concurrent same-key
		// admissions that picked different wallet inputs produce
		// different session ids and both miss the registry's
		// read-then-spawn dedup, then collide on the partial UNIQUE
		// index over idempotency_key. On SQLite the single writer
		// lock serializes the racing children, so the loser's lookup
		// here runs after the winner commits, sees the surviving row,
		// and consumes its turn cleanly (returning Existing). On
		// Postgres the children do not serialize on this table, so
		// both lookups can miss and the loser's snapshot upsert below
		// collides on the index; that collision is handled there as a
		// benign redelivery, and the redelivered turn dedups cleanly
		// here once the winner's row is visible. The gate only fires
		// for an outgoing admission carrying a key (resolveKeyDedup
		// short- circuits otherwise), where commit work does not
		// advance the FSM, so this pre-work snapshot is the final
		// state.
		gateRecord, err := b.snapshotRecord()
		if err != nil {
			return err
		}
		winner, deduped, err := b.resolveKeyDedup(txCtx, gateRecord)
		if err != nil {
			return err
		}
		if deduped {
			b.dedupKeyWinner = &winner

			return nil
		}

		// Run the staged commit work first (reservations, finalized
		// package, input-spend completion, incoming materialization).
		// Incoming materialization advances the FSM further inside its
		// closure, so the snapshot below must be taken AFTER this loop
		// so it reflects the final state. Each closure joins txCtx, so
		// it is atomic with the snapshot and the ack.
		for _, work := range b.commitWork {
			if err := work(txCtx, tx); err != nil {
				return err
			}
		}

		// Capture and persist the snapshot for the final FSM state. The
		// registry store joins txCtx (actor.TxFromContext), so this
		// lands atomically with the ack.
		record, err := b.snapshotRecord()
		if err != nil {
			return err
		}
		if err := b.cfg.RegistryStore.UpsertSession(
			txCtx, record,
		); err != nil {
			// On Postgres the resolveKeyDedup SELECT above can miss
			// a concurrent same-key winner (the racing children do
			// not serialize on oor_session_registry the way
			// SQLite's single writer does), so the loser's snapshot
			// collides on the partial UNIQUE index over
			// idempotency_key. Returning the error rolls the turn
			// back so it redelivers; the redelivered
			// resolveKeyDedup -- running in a fresh tx where the
			// winner's row is now committed and visible -- consumes
			// the turn cleanly as Existing. No special
			// classification is needed here: any commit error
			// redelivers, and the partial UNIQUE index is the
			// actual safety net against a duplicate row.
			return err
		}
		b.terminalCommitted = record.Status.IsTerminal()

		// Deliver the collected cross-actor transport messages directly
		// into the serverconn durable actor in the same transaction:
		// serverconn is durable, so each Tell persists into its mailbox
		// via the ambient txCtx and lands IFF the turn commits. The
		// wire send runs later on serverconn's own egress turn and is
		// retried by serverconn. Transport collected during commit work
		// (e.g. the incoming ack) is included because that work ran
		// above.
		if err := b.tellTransport(txCtx); err != nil {
			return err
		}

		// Ledger accounting Tells join the same transaction: the
		// ledger actor is durable, so each Tell persists the message
		// into its mailbox via the ambient txCtx and the entry lands
		// IFF the turn commits. Accounting collected during commit
		// work (incoming materialization) is included because that
		// work ran above.
		if err := b.tellLedger(txCtx); err != nil {
			return err
		}

		return nil
	})
}

// resolveKeyDedup detects, inside the commit's writer transaction, whether a
// concurrent same-idempotency-key admission already committed a surviving
// session with a different session id. It returns (winner, true, nil) when this
// child lost the race and should consume its turn as a dedup no-op, or
// (_, false, nil) when there is no conflict and the snapshot should be written.
// It only applies to a fresh outgoing admission carrying a non-empty key (the
// session id is the Ark txid, so a same-key session with a matching id is just
// this child's own row and never a conflict). The writer lock serializes racing
// children, so this read is race-free with respect to their commits.
func (b *sessionBehavior) resolveKeyDedup(ctx context.Context,
	record clientdb.OORSessionRegistryRecord) (SessionID, bool, error) {

	if record.Direction != clientdb.OORSessionDirectionOutgoing ||
		record.IdempotencyKey == "" {
		return SessionID{}, false, nil
	}

	store := b.cfg.RegistryStore
	existing, err := store.LookupActiveSessionByIdempotencyKey(
		ctx, record.IdempotencyKey,
	)
	switch {
	case errors.Is(err, clientdb.ErrOORSessionNotFound):
		return SessionID{}, false, nil

	case err != nil:
		return SessionID{}, false, err
	}

	// A surviving row with this child's own session id is not a conflict:
	// it is this session's prior committed snapshot being updated.
	winner := SessionID(existing.SessionID)
	if winner == b.sessionID {
		return SessionID{}, false, nil
	}

	b.logger(ctx).InfoS(ctx, "Idempotency-key race lost; deduping to "+
		"surviving session",
		slog.String("session_id", b.sessionID.String()),
		slog.String("winner_session_id", winner.String()),
		slog.String("idempotency_key", record.IdempotencyKey),
	)

	return winner, true, nil
}

// snapshotRecord builds the registry record for the current FSM state.
func (b *sessionBehavior) snapshotRecord() (clientdb.OORSessionRegistryRecord,
	error) {

	state, err := b.fsm.CurrentState()
	if err != nil {
		return clientdb.OORSessionRegistryRecord{}, err
	}

	switch b.direction {
	case clientdb.OORSessionDirectionOutgoing:
		outgoing, ok := state.(State)
		if !ok {
			return clientdb.OORSessionRegistryRecord{}, fmt.Errorf(
				"unexpected outgoing state "+
					"type: %T", state)
		}

		return outgoingRegistryRecord(b.sessionID, outgoing)

	case clientdb.OORSessionDirectionIncoming:
		return incomingRegistryRecord(b.sessionID, state)

	default:
		return clientdb.OORSessionRegistryRecord{}, fmt.Errorf(
			"unknown session direction: %d", b.direction)
	}
}

// restore rebuilds the FSM from the session's registry row, if one exists. A
// missing row means a brand-new session whose FSM is created on first
// StartTransfer/IncomingTransfer.
func (b *sessionBehavior) restore(ctx context.Context) error {
	record, err := b.cfg.RegistryStore.GetSession(
		ctx, chainHashOf(b.sessionID),
	)
	switch {
	case errors.Is(err, clientdb.ErrOORSessionNotFound):
		return nil

	case err != nil:
		return err
	}

	// Fail closed on any OOR flow version this build does not understand: a
	// session conducted under an unknown flow must not be resumed.
	if err := oorpb.ValidateFlowVersion(record.FlowVersion); err != nil {
		return err
	}

	// A direction conflict is only legal for the self-transfer
	// replacement: a fresh incoming session may be installed over a
	// terminal outgoing row (the registry defers the hint until the
	// outgoing session terminates). Anything else is a routing bug, not a
	// row to silently adopt. A zero requested direction means the caller
	// adopts whatever the row says.
	if b.direction != 0 && record.Direction != b.direction {
		selfTransfer := b.direction ==
			clientdb.OORSessionDirectionIncoming &&
			record.Direction ==
				clientdb.OORSessionDirectionOutgoing &&
			record.Status.IsTerminal()
		if !selfTransfer {
			return fmt.Errorf("session %s direction mismatch: "+
				"requested %d, stored %d", b.sessionID,
				b.direction, record.Direction)
		}

		// Leave the behavior unloaded: the incoming admission installs
		// a fresh receive session whose first commit overwrites the
		// terminal outgoing row.
		return nil
	}

	if len(record.SnapshotData) == 0 {
		return nil
	}

	switch record.Direction {
	case clientdb.OORSessionDirectionOutgoing:
		session, err := outgoingSessionFromRecord(ctx, *record)
		if err != nil {
			return err
		}
		b.setFSM(session.FSM)
		b.direction = clientdb.OORSessionDirectionOutgoing
		b.loaded = true

	case clientdb.OORSessionDirectionIncoming:
		session, err := incomingSessionFromRecord(
			ctx, *record, b.cfg.Limits,
		)
		if err != nil {
			return err
		}
		b.setFSM(session.FSM)
		b.direction = clientdb.OORSessionDirectionIncoming
		b.loaded = true

	default:
		return fmt.Errorf("unknown restored direction: %d",
			record.Direction)
	}

	b.logger(ctx).InfoS(ctx, "Restored OOR session FSM from registry row",
		slog.String("session_id", b.sessionID.String()),
		slog.Int("direction", int(record.Direction)),
		slog.String("phase", record.Phase),
	)

	return nil
}
