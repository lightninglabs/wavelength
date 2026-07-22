package credit

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/build"
	"github.com/lightninglabs/wavelength/db"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

const creditChildStopTimeout = 5 * time.Second

// Registry is the credit subsystem coordinator. It is a plain (non-durable)
// supervisor actor: it admits operations by writing their control-plane row in
// an ordinary transaction, spawns the per-operation durable child that owns the
// crash-safe execution, routes resume timers, reaps terminal children, and
// restores in-flight operations on boot. The durable row plus the per-operation
// child mailbox are the only durable state; the supervisor itself holds none.
type Registry struct {
	ref      actor.ActorRef[CreditMsg, CreditResp]
	tellRef  actor.TellOnlyRef[CreditMsg]
	actor    *actor.Actor[CreditMsg, CreditResp]
	behavior *registryBehavior
	redeemer *autoRedeemer
	wg       *sync.WaitGroup
}

// Ref returns the public registry actor reference.
func (r *Registry) Ref() actor.ActorRef[CreditMsg, CreditResp] {
	return r.ref
}

// TellRef returns a tell-only registry reference.
func (r *Registry) TellRef() actor.TellOnlyRef[CreditMsg] {
	return r.tellRef
}

// Stop stops the registry and every resident child.
func (r *Registry) Stop() {
	if r == nil || r.actor == nil {
		return
	}

	r.redeemer.stop()
	r.actor.Stop()
	if r.wg != nil {
		r.wg.Wait()
	}
}

// RestoreNonTerminal respawns and resumes every non-terminal operation. It runs
// as a registry message so the restore is serialized with concurrent
// admissions.
func (r *Registry) RestoreNonTerminal(ctx context.Context) error {
	if r == nil || r.ref == nil {
		return fmt.Errorf("credit registry must be provided")
	}

	_, err := r.ref.
		Ask(ctx, &RestoreNonTerminalRequest{}).
		Await(ctx).
		Unpack()

	return err
}

// NewRegistry creates and starts the credit registry coordinator.
func NewRegistry(cfg RegistryConfig) (*Registry, error) {
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
	if cfg.AdmitTimeout <= 0 {
		cfg.AdmitTimeout = DefaultAdmitTimeout
	}
	if cfg.ReceiveAdmitTimeout <= 0 {
		cfg.ReceiveAdmitTimeout = DefaultReceiveAdmitTimeout
	}

	earmark := &atomic.Pointer[EarmarkFunc]{}
	if cfg.AutoRedeem.EarmarkedSat != nil {
		fn := cfg.AutoRedeem.EarmarkedSat
		earmark.Store(&fn)
	}

	behavior := &registryBehavior{
		cfg:     cfg,
		log:     cfg.Log.UnwrapOr(btclog.Disabled),
		active:  make(map[string]*OpActor),
		earmark: earmark,
	}

	var wg sync.WaitGroup
	supervisor := actor.NewActor(actor.ActorConfig[CreditMsg, CreditResp]{
		ID:       CreditActorServiceKeyName,
		Behavior: behavior,
		Wg:       &wg,
	})
	behavior.selfRef = supervisor.TellRef()
	supervisor.Start()

	// Register under the credit service key so a timeout retry-callback ref
	// (built from the service key) resolves to this registry at Tell time.
	if cfg.ActorSystem != nil {
		err := actor.RegisterWithReceptionist(
			cfg.ActorSystem.Receptionist(), NewServiceKey(),
			supervisor.Ref(),
		)
		if err != nil {
			supervisor.Stop()
			wg.Wait()

			return nil, fmt.Errorf("register credit registry: %w",
				err)
		}
	}

	return &Registry{
		ref:      supervisor.Ref(),
		tellRef:  supervisor.TellRef(),
		actor:    supervisor,
		behavior: behavior,
		redeemer: newAutoRedeemer(cfg, supervisor.TellRef(), earmark),
		wg:       &wg,
	}, nil
}

// StartAutoRedeem runs the wallet-owned auto-redeem boot reconcile (a no-op
// when the policy is disabled). Steady-state auto-redeem is driven by the
// receive state machine; this only covers a balance already over the watermark
// at start. It is anchored to ctx, which must be a daemon-lifetime context, not
// an RPC-call context.
func (r *Registry) StartAutoRedeem(ctx context.Context) {
	if r == nil {
		return
	}

	r.redeemer.start(ctx)
}

// SetEarmarkProvider wires the credit-earmark provider auto-redeem consults to
// avoid redeeming credits an in-flight wallet operation is about to spend. It
// is safe to call after StartAutoRedeem: the wallet's prepared-send store is
// built after the registry, so the daemon wires this once that store exists.
func (r *Registry) SetEarmarkProvider(fn EarmarkFunc) {
	if r == nil {
		return
	}

	r.redeemer.setEarmark(fn)
}

// registryBehavior coordinates credit operations on a plain in-memory mailbox.
// Admissions write the operation row in an ordinary transaction; children are
// spawned and resumed afterwards, so a row always exists before any child turn
// and the boot-time restore can find every in-flight operation. A crash between
// the row write and the spawn is recovered by RestoreNonTerminal on the next
// boot.
type registryBehavior struct {
	cfg     RegistryConfig
	log     btclog.Logger
	selfRef actor.TellOnlyRef[CreditMsg]

	// active maps op id to its resident child. Only ever touched on the
	// registry goroutine (turns are serialized).
	active map[string]*OpActor

	// earmark is the shared credit-earmark provider every child and the
	// boot-time reconcile consult before deciding to auto-redeem. It is an
	// atomic pointer so the daemon can wire the provider after the registry
	// (and its resident children) are built, via SetEarmarkProvider,
	// without re-spawning anything.
	earmark *atomic.Pointer[EarmarkFunc]
}

// Compile-time check that registryBehavior is a plain actor behavior.
var _ actor.ActorBehavior[CreditMsg, CreditResp] = (*registryBehavior)(nil)

// Compile-time check that registryBehavior owns actor shutdown cleanup.
var _ actor.Stoppable = (*registryBehavior)(nil)

// logger returns the behavior logger bound to ctx.
func (b *registryBehavior) logger(ctx context.Context) btclog.Logger {
	if b.log != btclog.Disabled {
		return b.log
	}

	return build.LoggerFromContext(ctx)
}

// Receive routes one registry message. Admissions write their row and spawn the
// child; routing messages spawn/route/reap inline.
func (b *registryBehavior) Receive(ctx context.Context,
	msg CreditMsg) fn.Result[CreditResp] {

	switch m := msg.(type) {
	case *StartCreditPayRequest:
		return b.admit(ctx, m, m.OpKey)

	case *StartCreditReceiveRequest:
		return b.admit(ctx, m, m.OpKey)

	case *RedeemRequest:
		return b.admit(ctx, m, m.OpKey)

	case *ResumeCreditOpRequest:
		b.routeResume(ctx, m.OpID)

		return fn.Ok[CreditResp](&AckResponse{})

	case *ConsiderRedeemRequest:
		b.considerRedeem(ctx, m.AvailableSat)

		return fn.Ok[CreditResp](&AckResponse{})

	case *CreditTerminalNotification:
		b.reap(ctx, m.OpID)

		return fn.Ok[CreditResp](&AckResponse{})

	case *RestoreNonTerminalRequest:
		if err := b.restoreNonTerminal(ctx); err != nil {
			return fn.Err[CreditResp](err)
		}

		return fn.Ok[CreditResp](&AckResponse{})

	case *ListCreditOpsRequest:
		return b.handleList(ctx, m)

	default:
		return fn.Err[CreditResp](
			fmt.Errorf("unexpected credit registry message %T",
				msg),
		)
	}
}

// OnStop stops resident child actors from the registry actor goroutine and
// waits for their durable mailbox workers to exit. Keeping teardown inside the
// actor prevents shutdown from racing with terminal-child reaping, which also
// mutates the active child map.
func (b *registryBehavior) OnStop(ctx context.Context) error {
	return b.stopChildren(ctx)
}

// stopChild stops a terminal child and waits under a bounded context so reaping
// cannot wedge the registry supervisor indefinitely.
func (b *registryBehavior) stopChild(ctx context.Context, opID string,
	child *OpActor) {

	stopCtx, cancel := context.WithTimeout(
		context.WithoutCancel(ctx), creditChildStopTimeout,
	)
	defer cancel()

	if err := child.StopAndWait(stopCtx); err != nil {
		b.logger(ctx).WarnS(ctx, "Unable to stop credit child cleanly",
			err,
			slog.String("op_id", opID),
		)
	}
}

// reap stops and drops a child once its operation reaches a terminal status.
// The durable row is re-checked so a stale notification is harmless. Reaping
// waits for the child to exit under a short timeout, keeping actor ownership
// clear without letting one stuck child wedge the coordinator.
func (b *registryBehavior) reap(ctx context.Context, opID string) {
	rec, err := b.cfg.Store.GetOperation(ctx, opID)
	switch {
	case err == nil && !rec.Status.IsTerminal():
		// Not actually terminal; ignore the stale notification.
		return

	case err != nil && !errors.Is(err, db.ErrCreditOperationNotFound):
		b.logger(ctx).WarnS(ctx, "Unable to confirm credit terminal "+
			"state", err, slog.String("op_id", opID))

		return
	}

	if child, ok := b.active[opID]; ok {
		b.stopChild(ctx, opID, child)
		delete(b.active, opID)
		b.logger(ctx).InfoS(ctx, "Credit child reaped",
			slog.String("op_id", opID),
		)
	}
}

// admit dedups by op key and, for a fresh operation, writes the control-plane
// row before spawning and resuming the child. The write opens its own short
// transaction (the supervisor holds no ambient durable-actor transaction), so a
// crash after it commits is recovered by RestoreNonTerminal, and a crash before
// it commits is retried by the synchronous caller under the same stable op key.
func (b *registryBehavior) admit(ctx context.Context, msg CreditMsg,
	opKey string) fn.Result[CreditResp] {

	existing, err := b.cfg.Store.LookupActiveOperationByKey(ctx, opKey)
	switch {
	case err == nil:
		// Dedup hit: ensure the existing child is resident and
		// resuming, and answer with the known op id. A receive carries
		// its already-created invoice back so a retry is idempotent.
		resp := &StartCreditResponse{
			OpID:        existing.OpID,
			Existing:    true,
			Invoice:     existing.Invoice,
			PaymentHash: existing.PaymentHash,
		}
		b.routeResume(ctx, existing.OpID)

		return fn.Ok[CreditResp](resp)

	case errors.Is(err, db.ErrCreditOperationNotFound):
		// Fresh admission below.

	default:
		return fn.Err[CreditResp](
			fmt.Errorf("dedup credit op: %w", err),
		)
	}

	opID, err := newOpID()
	if err != nil {
		return fn.Err[CreditResp](err)
	}

	rec, snap := buildRecord(opID, msg)
	if rec == nil {
		return fn.Err[CreditResp](
			fmt.Errorf("unsupported admission %T", msg),
		)
	}

	// A credit receive creates its server-owned invoice synchronously,
	// before the row is written, so the wallet can return the invoice from
	// the same Send/Recv call. CreateCredit is idempotent by op key, so a
	// crash before the row commits replays it on the caller's retry. Pay
	// and redeem carry no admission-time work.
	if rec.Kind == KindReceive {
		if err := b.createReceiveInvoice(
			ctx, rec, snap.Memo,
		); err != nil {
			return fn.Err[CreditResp](err)
		}
	}

	raw, err := snap.encode()
	if err != nil {
		return fn.Err[CreditResp](err)
	}
	rec.SnapshotData = raw
	rec.SnapshotVersion = snapshotVersion

	if err := b.cfg.Store.UpsertOperation(ctx, *rec); err != nil {
		return fn.Err[CreditResp](
			fmt.Errorf("write credit op row: %w", err),
		)
	}

	b.logger(ctx).InfoS(ctx, "Credit operation admitted",
		slog.String("op_id", opID),
		slog.String("op_key", opKey),
		slog.Int("kind", int(rec.Kind)),
	)

	resp := &StartCreditResponse{
		OpID:        opID,
		Existing:    false,
		Invoice:     rec.Invoice,
		PaymentHash: rec.PaymentHash,
	}
	b.routeResume(ctx, opID)

	return fn.Ok[CreditResp](resp)
}

// createReceiveInvoice creates the server-owned Lightning receive invoice for a
// fresh receive admission and advances the record straight to awaiting
// settlement, so the spawned child only has to reconcile the credit against the
// server ledger. The invoice is returned to the caller in the admission
// response.
//
// This is the one server round-trip an admission makes on the supervisor
// goroutine, which serializes every admission, resume, and reap. It is bounded
// by AdmitTimeout so one slow or hung receive cannot head-of-line-block the
// whole subsystem; on timeout the synchronous caller retries under the same
// stable op key, and CreateCredit is idempotent, so the retry reuses the same
// invoice.
func (b *registryBehavior) createReceiveInvoice(ctx context.Context,
	rec *db.CreditOperationRecord, memo string) error {

	acctKey, err := b.cfg.Daemon.IdentityPubKey(ctx)
	if err != nil {
		return fmt.Errorf("get identity pubkey: %w", err)
	}

	callCtx, cancel := context.WithTimeout(ctx, b.cfg.ReceiveAdmitTimeout)
	defer cancel()

	res, err := b.cfg.Server.CreateCredit(
		callCtx, acctKey, rec.OpKey, SourceLightningReceive,
		uint64(rec.AmountSat), memo,
	)
	if err != nil {
		// Log at the exact failure site: this is the single synchronous
		// swap-server round-trip a sub-dust receive makes, and when the
		// operator's credit subsystem is unresponsive it is otherwise
		// completely invisible (wavelength#1041).
		b.logger(ctx).WarnS(ctx, "Credit receive CreateCredit failed",
			err,
			slog.String("op_key", rec.OpKey),
			slog.Int64("amount_sat", rec.AmountSat),
		)

		return fmt.Errorf("create receive credit: %w", err)
	}
	if res.State.IsTerminalFailure() {
		return fmt.Errorf("receive credit ended in %s", res.State)
	}
	if res.Invoice == "" {
		return fmt.Errorf("receive credit missing invoice")
	}

	rec.ServerOpID = res.OperationID
	rec.Invoice = res.Invoice
	if len(res.PaymentHash) > 0 {
		rec.PaymentHash = append([]byte(nil), res.PaymentHash...)
	}
	rec.State = string(StateAwaitingSettlement)
	rec.Status = StateAwaitingSettlement.Status()

	return nil
}

// routeResume ensures a child for opID is resident and tells it to resume. An
// unknown or terminal operation is a benign no-op.
func (b *registryBehavior) routeResume(ctx context.Context, opID string) {
	child, err := b.ensureChild(ctx, opID)
	if err != nil {
		b.logger(ctx).WarnS(ctx, "Unable to ensure credit child",
			err,
			slog.String("op_id", opID),
		)

		return
	}
	if child == nil {
		return
	}

	err = child.TellRef().Tell(ctx, &ResumeCreditOpRequest{OpID: opID})
	if err != nil {
		b.logger(ctx).DebugS(ctx, "Unable to resume credit child",
			slog.String("op_id", opID),
			slog.String("err", err.Error()),
		)
	}
}

// ensureChild returns the resident child for opID, spawning it from the durable
// row when not resident. Returns nil for an unknown or terminal operation.
func (b *registryBehavior) ensureChild(ctx context.Context, opID string) (
	*OpActor, error) {

	if child, ok := b.active[opID]; ok {
		return child, nil
	}

	rec, err := b.cfg.Store.GetOperation(ctx, opID)
	if errors.Is(err, db.ErrCreditOperationNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if rec.Status.IsTerminal() {
		return nil, nil
	}

	// The child actor is daemon-owned: its construction-time restore must
	// not inherit the turn ctx that triggered this lazy spawn, or a
	// canceled request could abort a child the daemon still owns.
	//nolint:contextcheck
	child, err := NewOpActor(b.childConfig(opID))
	if err != nil {
		return nil, err
	}
	b.active[opID] = child

	return child, nil
}

// restoreNonTerminal respawns and resumes every non-terminal operation.
func (b *registryBehavior) restoreNonTerminal(ctx context.Context) error {
	rows, err := b.cfg.Store.ListNonTerminal(ctx)
	if err != nil {
		return fmt.Errorf("list non-terminal credit ops: %w", err)
	}

	for i := range rows {
		b.routeResume(ctx, rows[i].OpID)
	}

	b.logger(ctx).InfoS(ctx, "Restored non-terminal credit operations",
		slog.Int("count", len(rows)),
	)

	return nil
}

// handleList projects the stored operations into compact summaries. A
// pending-only request reads just the non-terminal rows; a full request reads
// every row so the wallet projector can observe terminal transitions.
func (b *registryBehavior) handleList(ctx context.Context,
	m *ListCreditOpsRequest) fn.Result[CreditResp] {

	var (
		rows []db.CreditOperationRecord
		err  error
	)
	if m.PendingOnly {
		rows, err = b.cfg.Store.ListNonTerminal(ctx)
	} else {
		rows, err = b.cfg.Store.ListOperations(ctx)
	}
	if err != nil {
		return fn.Err[CreditResp](err)
	}

	resp := &ListCreditOpsResponse{}
	for i := range rows {
		rec := rows[i]

		// The credit-only marker lives in the resume snapshot blob, so
		// decode it here to surface it in the summary. A malformed blob
		// is treated as not-credit-only rather than failing the whole
		// list.
		var creditOnly bool
		if snap, derr := decodeOpSnapshot(
			rec.SnapshotData,
		); derr == nil {

			creditOnly = snap.CreditOnly
		}

		resp.Ops = append(resp.Ops, CreditOpSummary{
			OpID:         rec.OpID,
			OpKey:        rec.OpKey,
			Kind:         rec.Kind,
			State:        State(rec.State),
			Pending:      !rec.Status.IsTerminal(),
			AmountSat:    rec.AmountSat,
			CreditOnly:   creditOnly,
			OORSessionID: rec.OORSessionID,
			TopupSat:     rec.TopupSat,
			LastError:    rec.LastError,
		})
	}

	return fn.Ok[CreditResp](resp)
}

// considerRedeem materializes the supplied available balance into a vTXO when
// the no-pending-pay/redeem interlock allows it. A receive that cleared the
// auto-redeem watermark (or the boot-time reconcile) signals the amount; this
// applies the interlock the receive FSM does not own — a pending pay may be
// about to consume credits, and a pending redeem already owns the materialize —
// and admits a fresh redeem operation when neither is in flight. The redeem op
// key is random, so it always admits a new operation rather than deduping.
func (b *registryBehavior) considerRedeem(ctx context.Context,
	available uint64) {

	if available == 0 {
		return
	}

	ops, err := b.cfg.Store.ListNonTerminal(ctx)
	if err != nil {
		// Dropping the signal leaves the credits available and safe,
		// but nothing re-evaluates them until the next receive trigger
		// or a restart, so log loudly rather than silently skipping.
		b.logger(ctx).WarnS(ctx, "Unable to evaluate auto-redeem", err)

		return
	}
	for i := range ops {
		switch ops[i].Kind {
		case KindPay, KindRedeem:
			// A pending pay may consume the credits, and a pending
			// redeem already owns the materialize, so defer to it.
			// The over-watermark balance is then re-evaluated only
			// at the next receive trigger or a restart's boot
			// reconcile, not the moment this blocking op clears.
			// Closing that gap causally is the single-authority
			// follow-up that routes credit-backed send reservations
			// through this registry.
			b.logger(ctx).DebugS(ctx, "Deferring auto-redeem; an "+
				"operation is in flight",
				slog.String("blocking_op", ops[i].OpID),
				slog.Int("kind", int(ops[i].Kind)),
			)

			return

		case KindReceive:
			// A pending receive only adds credits, so it does not
			// block an auto-redeem.
		}
	}

	opKey, err := redeemOpKey()
	if err != nil {
		b.logger(ctx).WarnS(ctx, "Unable to mint redeem op key", err)

		return
	}

	b.logger(ctx).InfoS(ctx, "Auto-redeeming available credits",
		slog.Uint64("amount_sat", available),
	)

	// The amount is the available balance the trigger observed, which may
	// be slightly stale by the time it reserves. That is safe: the server
	// revalidates the reservation in redeemSubmittingState, so an amount
	// the balance no longer supports fails the redeem op cleanly rather
	// than over-materializing.
	//
	// admit can also fail to write the control-plane row; surface it, since
	// the redeem is then dropped until the next receive trigger or a
	// restart's boot reconcile re-derives the signal.
	if err := b.admit(
		ctx, &RedeemRequest{OpKey: opKey, AmountSat: available}, opKey,
	).Err(); err != nil {

		b.logger(ctx).WarnS(ctx, "Auto-redeem admission failed; "+
			"deferring to boot reconcile", err,
			slog.Uint64("amount_sat", available),
		)
	}
}

// childConfig builds the per-operation actor config for one op id.
func (b *registryBehavior) childConfig(opID string) OpActorConfig {
	return OpActorConfig{
		OpID:              opID,
		Log:               b.cfg.Log,
		Server:            b.cfg.Server,
		Daemon:            b.cfg.Daemon,
		Store:             b.cfg.Store,
		DeliveryStore:     b.cfg.DeliveryStore,
		TimeoutActor:      b.cfg.TimeoutActor,
		CallbackRef:       b.cfg.CallbackRef,
		Registry:          b.selfRef,
		PollInterval:      b.cfg.PollInterval,
		MaxAwaitingPolls:  b.cfg.MaxAwaitingPolls,
		AutoRedeemEnabled: b.cfg.AutoRedeem.Enabled,
		MinRedeemSat:      b.cfg.AutoRedeem.MinRedeemSat,
		Earmark:           b.earmark,
	}
}

// stopChildren stops every resident child on teardown.
func (b *registryBehavior) stopChildren(ctx context.Context) error {
	for _, child := range b.active {
		child.Stop()
	}

	var stopErr error
	for opID, child := range b.active {
		if err := child.Wait(ctx); err != nil {
			stopErr = errors.Join(
				stopErr, fmt.Errorf("stop credit child %s: %w",
					opID, err),
			)
		}
		delete(b.active, opID)
	}

	return stopErr
}

// newOpID mints a fresh random operation id.
func newOpID() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("generate credit op id: %w", err)
	}

	return hex.EncodeToString(buf[:]), nil
}
