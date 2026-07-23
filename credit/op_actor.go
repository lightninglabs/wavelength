package credit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/build"
	"github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/timeout"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

// ActorIDForOp returns the deterministic durable mailbox id for one credit
// operation. It is derived from the stable op id so the same mailbox is used
// across restarts.
func ActorIDForOp(opID string) string {
	return "credit-op-" + opID
}

// OpActor wraps one durable per-operation credit actor.
type OpActor struct {
	ref     actor.ActorRef[CreditDurableMsg, CreditResp]
	tellRef actor.TellOnlyRef[CreditDurableMsg]
	durable *actor.DurableActor[CreditDurableMsg, CreditResp]
}

// Ref returns the public actor reference.
func (a *OpActor) Ref() actor.ActorRef[CreditDurableMsg, CreditResp] {
	return a.ref
}

// TellRef returns a tell-only reference.
func (a *OpActor) TellRef() actor.TellOnlyRef[CreditDurableMsg] {
	return a.tellRef
}

// Stop stops the underlying durable actor.
func (a *OpActor) Stop() {
	if a == nil || a.durable == nil {
		return
	}

	a.durable.Stop()
}

// StopAndWait stops the underlying durable actor and waits for its worker loop
// to exit.
func (a *OpActor) StopAndWait(ctx context.Context) error {
	if a == nil || a.durable == nil {
		return nil
	}

	return a.durable.StopAndWait(ctx)
}

// Wait blocks until the underlying durable actor exits.
func (a *OpActor) Wait(ctx context.Context) error {
	if a == nil || a.durable == nil {
		return nil
	}

	return a.durable.Wait(ctx)
}

// NewOpActor creates and starts one durable per-operation credit actor,
// restoring its state from the credit_operations row before the mailbox starts
// draining.
func NewOpActor(cfg OpActorConfig) (*OpActor, error) {
	if cfg.OpID == "" {
		return nil, fmt.Errorf("op id must be provided")
	}
	if cfg.Store == nil {
		return nil, fmt.Errorf("credit store must be provided")
	}
	if cfg.DeliveryStore == nil {
		return nil, fmt.Errorf("delivery store must be provided")
	}
	if cfg.Server == nil {
		return nil, fmt.Errorf("credit server must be provided")
	}
	if cfg.Daemon == nil {
		return nil, fmt.Errorf("credit daemon must be provided")
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = DefaultPollInterval
	}

	actorID := ActorIDForOp(cfg.OpID)
	behavior := &opBehavior{
		cfg:     cfg,
		actorID: actorID,
		log:     cfg.Log.UnwrapOr(btclog.Disabled),
	}
	if err := behavior.restore(context.Background()); err != nil {
		return nil, err
	}

	durableCfg := actor.DefaultDurableTxActorConfig[
		CreditDurableMsg, CreditResp, creditTx,
	](
		actorID, behavior, behavior.bindStores, cfg.DeliveryStore,
		NewCodec(),
	)
	durableCfg.Log = cfg.Log

	durable, err := actor.NewDurableActor(durableCfg).Unpack()
	if err != nil {
		return nil, err
	}
	behavior.selfRef = durable.TellRef()
	durable.Start()

	return &OpActor{
		ref:     durable.Ref(),
		tellRef: durable.TellRef(),
		durable: durable,
	}, nil
}

// creditTx is the transaction-scoped store handed to the op behavior inside
// each Read/Stage/Commit phase. The control-plane snapshot write joins the same
// framework transaction via the behavior's Store, which reads the *sql.Tx off
// the closure context.
type creditTx struct {
	store   actor.DeliveryStore
	actorID string
}

// opBehavior runs on the durable actor Read/Stage/Commit path for one credit
// operation. Every external call (CreateCredit, SendOOR, ListCredits, StartPay,
// RedeemCredit, VTXO lookup) is idempotent by the op key or the invoice payment
// hash, so the behavior runs them inline in dispatch with no writer held and
// folds only the control-plane snapshot write plus the lease-fenced ack into
// commitAck.
type opBehavior struct {
	cfg     OpActorConfig
	actorID string
	log     btclog.Logger
	selfRef actor.TellOnlyRef[CreditDurableMsg]

	// rec is the current in-memory control-plane record, lazily restored or
	// created on admission.
	rec *db.CreditOperationRecord

	// redeemPkScript is the wallet-owned receive pkScript a redemption
	// watches, decoded from the snapshot blob.
	redeemPkScript []byte

	// receiveMemo is the memo carried from a receive admission message into
	// the first ReceiveCreating turn. It is not persisted: the admission
	// message is durable and redelivered until that turn commits.
	receiveMemo string

	// creditOnly marks a pay that settles entirely from credit with no
	// Lightning swap leg. It is round-tripped through the snapshot blob so
	// a terminal row keeps the marker the wallet projector reads.
	creditOnly bool

	// awaitPolls counts reconciliation polls taken in the current awaiting
	// state, checked against cfg.MaxAwaitingPolls. Reset to zero whenever
	// the FSM advances into a different state.
	awaitPolls uint32

	// acctKey caches the wallet identity pubkey that keys the credit
	// account.
	acctKey []byte

	// armRetry reports that this turn ended in an awaiting state and a poll
	// timer should be armed after commit.
	armRetry bool

	// terminalCommitted reports that the committed snapshot reached a
	// terminal status, consumed by the registry reap notification.
	terminalCommitted bool

	// commitFailed reports that the previous turn advanced rec in memory
	// but its Commit rolled back, so the next driving turn re-loads from
	// the durable row before dispatch.
	commitFailed bool

	// curState is the live protofsm state this operation is in. It is
	// reconstructed from the persisted state string on restore and advanced
	// by each transition the driving loop runs, kept in lockstep with
	// rec.State (the persisted mirror) via applyState.
	curState CreditState

	// corruptState records that the persisted state string did not decode
	// to a known FSM state, so the next dispatch drives the row to a
	// durable failure instead of treating the fallback failedState as
	// already committed.
	corruptState bool

	// pendingRedeem holds an auto-redeem trigger a settled receive emitted
	// this turn, fired at the registry after the turn commits so a crash
	// before it never leaves a half-applied redeem. Reset at the start of
	// every drive.
	pendingRedeem *triggerRedeem
}

// Compile-time check that opBehavior runs on the Read/Stage/Commit path.
var _ actor.TxBehavior[
	CreditDurableMsg, CreditResp, creditTx,
] = (*opBehavior)(nil)

// bindStores is the StoreFactory for the op Read/Stage/Commit path.
func (b *opBehavior) bindStores(_ context.Context,
	ds actor.DeliveryStore) creditTx {

	return creditTx{
		store:   ds,
		actorID: b.actorID,
	}
}

// logger returns the behavior logger bound to ctx.
func (b *opBehavior) logger(ctx context.Context) btclog.Logger {
	if b.log != btclog.Disabled {
		return b.log
	}

	return build.LoggerFromContext(ctx)
}

// restore rebuilds the in-memory record from the durable row, if one exists.
func (b *opBehavior) restore(ctx context.Context) error {
	rec, err := b.cfg.Store.GetOperation(ctx, b.cfg.OpID)
	if errors.Is(err, db.ErrCreditOperationNotFound) {
		return nil
	}
	if err != nil {
		return err
	}

	snap, err := decodeOpSnapshot(rec.SnapshotData)
	if err != nil {
		return err
	}

	b.rec = rec
	b.redeemPkScript = snap.RedeemPkScript
	b.receiveMemo = snap.Memo
	b.creditOnly = snap.CreditOnly
	b.awaitPolls = snap.AwaitPolls

	// Reconstruct the live protofsm state from the persisted state string
	// so a resumed operation re-enters its FSM exactly where the durable
	// row left off. An unrecognized string is flagged so the next dispatch
	// fails the corrupt row durably rather than wedging it.
	curState, known := decodeCreditState(State(rec.State))
	b.curState = curState
	b.corruptState = !known

	return nil
}

// Receive is the single entry point for every message that drives this
// operation. Read-only probes return directly; everything else drains the FSM
// in dispatch and is consumed by one lease-fenced commitAck.
func (b *opBehavior) Receive(ctx context.Context, msg CreditDurableMsg,
	ax actor.Exec[creditTx]) fn.Result[CreditResp] {

	if _, ok := msg.(*actor.RestartMessage); ok {

		// Restore already ran at construction; nothing to persist.
		return fn.Ok[CreditResp](&AckResponse{})
	}

	// If the previous turn's Commit rolled back after advancing rec in
	// memory, reload from the durable row before dispatch so the
	// redelivered event re-applies against the last-committed state.
	if b.commitFailed {
		if err := b.restore(ctx); err != nil {
			return fn.Err[CreditResp](err)
		}
		b.commitFailed = false
	}

	if err := b.dispatch(ctx, msg, ax); err != nil {
		// A mid-turn failure (a failed Stage checkpoint or a step error
		// after setState) may have advanced rec in memory past the last
		// durable checkpoint. Force a reload before the next turn so
		// redelivery re-drives from durable truth, not a stale
		// in-memory advance. Effects re-run from the reloaded state
		// stay safe because each is idempotent by the op key or payment
		// hash.
		b.commitFailed = true

		return fn.Err[CreditResp](err)
	}

	if b.rec == nil {

		// Unknown operation (resume for a row that no longer exists);
		// ack as a benign no-op.
		return fn.Ok[CreditResp](&AckResponse{})
	}

	if err := b.commitAck(ctx, ax); err != nil {
		b.commitFailed = true

		return fn.Err[CreditResp](err)
	}

	b.afterCommit(ctx)

	return fn.Ok[CreditResp](&AckResponse{})
}

// dispatch drives the already-loaded record forward until it reaches a terminal
// or awaiting state. The supervisor pre-writes the control-plane row before
// spawning a child and drives it with ResumeCreditOpRequest, so a child never
// admits its own operation: it always restores from the durable row at
// construction and re-drives from there.
func (b *opBehavior) dispatch(ctx context.Context, msg CreditDurableMsg,
	ax actor.Exec[creditTx]) error {

	b.armRetry = false
	b.pendingRedeem = nil

	switch msg.(type) {
	case *ResumeCreditOpRequest:
		// Drive the already-loaded record; nothing to admit.

	default:
		return fmt.Errorf("unexpected credit message %T", msg)
	}

	if b.rec == nil {
		return nil
	}
	if b.curState == nil {
		curState, known := decodeCreditState(State(b.rec.State))
		b.curState = curState
		b.corruptState = !known
	}

	// A persisted state that did not decode to a known FSM state is a
	// corrupt row. Drive it to a durable failure so it terminal-commits and
	// is reaped, rather than treating the in-memory failed fallback as
	// already committed and leaving the row non-terminal forever.
	if b.corruptState {
		return b.failCorrupt(ctx)
	}
	if b.curState.IsTerminal() {
		return nil
	}

	return b.runFSM(ctx, ax)
}

// failCorrupt drives an operation whose persisted state string did not decode
// to a known FSM state into a durable terminal failure. It mirrors the failed
// state onto the record so the turn's commit persists StateFailed and the
// registry reaps the row, rather than leaving a non-terminal row that
// restoreNonTerminal would respawn on every boot without ever being able to
// advance it.
func (b *opBehavior) failCorrupt(ctx context.Context) error {
	reason := fmt.Sprintf("unrecognized persisted state %q", b.rec.State)
	b.logger(ctx).WarnS(ctx, "Failing corrupt credit operation",
		nil,
		slog.String("op_id", b.cfg.OpID),
		slog.String("state", b.rec.State),
	)

	b.rec.LastError = reason
	b.applyState(&failedState{})
	b.curState = &failedState{}
	b.corruptState = false

	return nil
}

// buildRecord constructs the durable admission record and resume snapshot for
// one credit operation from its admission message. It is the single source of
// record construction, shared by the supervisor's admit path and the unit tests
// that drive a child directly, so both produce identical rows.
func buildRecord(opID string,
	msg CreditMsg) (*db.CreditOperationRecord, *opSnapshot) {

	switch m := msg.(type) {
	case *StartCreditPayRequest:
		return &db.CreditOperationRecord{
			OpID:         opID,
			OpKey:        m.OpKey,
			Kind:         KindPay,
			State:        string(initialState(KindPay)),
			Status:       db.CreditOpStatusPending,
			PaymentHash:  append([]byte(nil), m.PaymentHash[:]...),
			Invoice:      m.Invoice,
			AmountSat:    int64(m.AmountSat),
			TopupSat:     int64(m.TopupSat),
			MaxCreditSat: int64(m.MaxCreditSat),
			MaxFeeSat:    int64(m.MaxFeeSat),
			RoutingFeeBudgetSat: int64(
				m.RoutingFeeBudgetSat,
			),
		}, &opSnapshot{CreditOnly: m.CreditOnly}

	case *StartCreditReceiveRequest:
		return &db.CreditOperationRecord{
			OpID:      opID,
			OpKey:     m.OpKey,
			Kind:      KindReceive,
			State:     string(initialState(KindReceive)),
			Status:    db.CreditOpStatusPending,
			AmountSat: int64(m.AmountSat),
		}, &opSnapshot{Memo: m.Memo}

	case *RedeemRequest:
		return &db.CreditOperationRecord{
			OpID:      opID,
			OpKey:     m.OpKey,
			Kind:      KindRedeem,
			State:     string(initialState(KindRedeem)),
			Status:    db.CreditOpStatusPending,
			AmountSat: int64(m.AmountSat),
		}, &opSnapshot{}

	default:
		return nil, &opSnapshot{}
	}
}

// runFSM advances the operation forward until it reaches a terminal state or an
// awaiting state that must park on a poll timer.
func (b *opBehavior) runFSM(ctx context.Context,
	ax actor.Exec[creditTx]) error {

	for {
		if b.curState.IsTerminal() {
			return nil
		}

		transition, err := b.curState.ProcessEvent(ctx, &opDrive{}, b)
		if err != nil {
			return err
		}

		// Mirror the next state onto the durable record before flushing
		// any checkpoint, so a Stage write persists the advanced state.
		b.applyState(transition.NextState)
		b.curState = transition.NextState

		var outbox []CreditOutMsg
		transition.NewEvents.WhenSome(func(ev CreditEmittedEvent) {
			outbox = ev.Outbox
		})

		park := false
		for _, out := range outbox {
			switch o := out.(type) {
			case *stageRecord:
				if serr := b.checkpoint(ctx, ax); serr != nil {
					return serr
				}

			case *parkOp:
				park = true

			case *triggerRedeem:
				b.pendingRedeem = o
			}
		}

		if park {
			b.armRetry = true

			return nil
		}
	}
}

// applyState mirrors the FSM's next protofsm state onto the durable record's
// state and status columns. Entering a different state resets the awaiting-poll
// counter so each awaiting state gets a fresh poll budget.
func (b *opBehavior) applyState(next CreditState) {
	s := State(next.String())
	if b.rec.State != string(s) {
		b.awaitPolls = 0
	}
	b.rec.State = string(s)
	b.rec.Status = s.Status()
}

// checkpoint durably persists the current record via a Stage write, so a server
// identifier recorded this turn survives a crash before the next side-effect
// call. Staging keeps the persist-before-effect invariant without holding the
// writer across the following IO; the message is still consumed once, at the
// final commit.
func (b *opBehavior) checkpoint(ctx context.Context,
	ax actor.Exec[creditTx]) error {

	return ax.Stage(ctx, func(stageCtx context.Context, _ creditTx) error {
		return b.persist(stageCtx)
	})
}

// awaitExhausted records one reconciliation poll for the current awaiting state
// and reports whether the configured poll cap has been exceeded. A zero cap
// means unlimited: the wait is bounded only by the server-reported terminal
// states.
func (b *opBehavior) awaitExhausted() bool {
	b.awaitPolls++

	limit := b.cfg.MaxAwaitingPolls

	return limit > 0 && b.awaitPolls > limit
}

// accountKey returns the cached wallet identity pubkey that keys the account.
func (b *opBehavior) accountKey(ctx context.Context) ([]byte, error) {
	if len(b.acctKey) > 0 {
		return b.acctKey, nil
	}

	key, err := b.cfg.Daemon.IdentityPubKey(ctx)
	if err != nil {
		return nil, fmt.Errorf("get identity pubkey: %w", err)
	}
	b.acctKey = key

	return key, nil
}

// commitAck folds the control-plane snapshot write and the lease-fenced ack
// into one short transaction.
func (b *opBehavior) commitAck(ctx context.Context,
	ax actor.Exec[creditTx]) error {

	terminal := State(b.rec.State).IsTerminal()

	err := ax.Commit(
		ctx,
		func(commitCtx context.Context, _ creditTx) error {
			return b.persist(commitCtx)
		},
	)
	if err != nil {
		return err
	}

	b.terminalCommitted = terminal

	return nil
}

// persist writes the current record snapshot, joining the ambient framework
// transaction via commitCtx.
func (b *opBehavior) persist(ctx context.Context) error {
	snap := &opSnapshot{
		RedeemPkScript: b.redeemPkScript,
		Memo:           b.receiveMemo,
		CreditOnly:     b.creditOnly,
		AwaitPolls:     b.awaitPolls,
	}
	raw, err := snap.encode()
	if err != nil {
		return err
	}

	rec := *b.rec
	rec.SnapshotData = raw
	rec.SnapshotVersion = snapshotVersion

	return b.cfg.Store.UpsertOperation(ctx, rec)
}

// afterCommit arms the poll timer and notifies the registry of a terminal
// operation. Both are best-effort: a crash in this window re-arms on boot via
// RestoreNonTerminal, and the registry re-checks the durable row before
// reaping.
func (b *opBehavior) afterCommit(ctx context.Context) {
	// A settled receive that cleared the auto-redeem watermark signals the
	// registry now, after its terminal snapshot committed, so a crash in
	// this window leaves no half-applied redeem: nothing is staged, the
	// credits stay available, and the next receive trigger or the boot-time
	// reconcile re-derives the signal.
	if b.pendingRedeem != nil {
		b.considerRedeem(ctx, b.pendingRedeem.AvailableSat)
		b.pendingRedeem = nil
	}

	if b.terminalCommitted {
		b.notifyTerminal(ctx)

		return
	}

	if b.armRetry {
		b.armPollTimer(ctx)
	}
}

// considerRedeem asks the registry to materialize the supplied available
// balance into a vTXO, now that a settled receive cleared the auto-redeem
// watermark. The registry arbitrates the no-pending-pay/redeem interlock, so a
// stale or redundant signal is harmless. Best-effort: a missed signal is
// re-derived by the next receive trigger or the boot-time reconcile.
func (b *opBehavior) considerRedeem(ctx context.Context, available uint64) {
	if b.cfg.Registry == nil {
		return
	}

	notifyCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	err := b.cfg.Registry.Tell(notifyCtx, &ConsiderRedeemRequest{
		AvailableSat: available,
	})
	if err != nil {
		// A dropped signal leaves the credits available and safe; the
		// boot-time reconcile re-derives it on the next start, but not
		// before, so log loudly rather than silently degrading.
		b.
			logger(ctx).
			WarnS(
				ctx,
				"Unable to signal credit auto-redeem; "+
					"deferring to boot reconcile",
				err,
				slog.String("op_id", b.cfg.OpID),
			)
	}
}

// armPollTimer schedules a poll timer that re-pokes this operation after the
// configured backoff so an awaiting state reconciles against the server ledger
// or the chain without a hot loop.
func (b *opBehavior) armPollTimer(ctx context.Context) {
	if b.cfg.TimeoutActor == nil || b.cfg.CallbackRef == nil {
		return
	}

	err := b.cfg.TimeoutActor.Tell(ctx, &timeout.ScheduleTimeoutRequest{
		ID:       timeout.ID(b.cfg.OpID),
		Duration: b.cfg.PollInterval,
		Callback: b.cfg.CallbackRef,
	})
	if err != nil {
		b.logger(ctx).DebugS(ctx, "Unable to arm credit poll timer",
			slog.String("op_id", b.cfg.OpID),
			slog.String("err", err.Error()),
		)
	}
}

// notifyTerminal tells the registry this operation reached a terminal status so
// the child can be reaped.
func (b *opBehavior) notifyTerminal(ctx context.Context) {
	if b.cfg.Registry == nil {
		return
	}

	notifyCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	err := b.cfg.Registry.Tell(notifyCtx, &CreditTerminalNotification{
		OpID: b.cfg.OpID,
	})
	if err != nil {
		b.
			logger(ctx).
			DebugS(
				ctx,
				"Unable to notify credit registry of "+
					"terminal op",
				slog.String("op_id", b.cfg.OpID),
				slog.String("err", err.Error()),
			)
	}
}
