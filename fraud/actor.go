package fraud

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/chainsource"
	"github.com/lightninglabs/wavelength/lib/actormsg"
	"github.com/lightninglabs/wavelength/vtxo"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

const (
	// Subsystem is the logging subsystem label for recipient fraud watches.
	Subsystem = "CFRD"

	// ServiceKeyName is the receptionist key for the recipient fraud
	// watcher.
	ServiceKeyName = "recipient-fraud-watcher"
)

// ServiceKey returns the actor service key for the recipient fraud
// watcher. Callers that need to look the watcher up via the
// receptionist should use this rather than constructing the key
// themselves; it locks down a single concrete instantiation site.
func ServiceKey() actor.ServiceKey[Msg, Resp] {
	return actor.NewServiceKey[Msg, Resp](ServiceKeyName)
}

// WatcherConfig configures the recipient fraud watcher actor.
type WatcherConfig struct {
	// ChainSource provides passive ancestor spend watches.
	ChainSource actor.ActorRef[
		chainsource.ChainSourceMsg, chainsource.ChainSourceResp,
	]

	// VTXOManagerRef drives affected targets into unilateral exit through
	// the VTXO manager's single admission gate. The manager transitions
	// the VTXO to UnilateralExitState (persisting it out of the live set)
	// and emits the chain-resolver notification that starts the durable
	// unroll job under TriggerFraudSpend, so fraud escalation converges on
	// the same path as manual and critical-expiry exits rather than
	// admitting the registry job behind the manager's back.
	VTXOManagerRef actor.ActorRef[vtxo.ManagerMsg, vtxo.ManagerResp]

	// Log is an optional logger.
	Log fn.Option[btclog.Logger]

	// MailboxSize overrides the watcher's actor mailbox capacity.
	// Zero or negative applies defaultWatcherMailboxSize. Sized to
	// absorb a burst of per-target spend notifications during a
	// chain reorganization without blocking the chainsource
	// publisher.
	MailboxSize int
}

// defaultWatcherMailboxSize is the fallback mailbox capacity applied
// when WatcherConfig.MailboxSize is zero or negative. Sized large
// enough to absorb a burst of per-target spend notifications during a
// chain reorganization without blocking the chainsource publisher.
const defaultWatcherMailboxSize = 64

// trackedTarget records the watch plan a recipient VTXO is currently
// armed under.
type trackedTarget struct {
	plan *WatchPlan
}

// WatcherActor is the recipient fraud watcher: a passive ancestor-spend
// monitor that forces the affected target into unilateral exit through the
// VTXO manager (ForceUnrollRequest under TriggerFraudSpend) when any watched
// ancestor of a tracked OOR VTXO is observed spent on chain. It is both the
// public handle (Ref / Stop) and the
// actor.ActorBehavior implementation; the runtime is driven through
// the embedded actor.
type WatcherActor struct {
	cfg     WatcherConfig
	log     btclog.Logger
	selfRef actor.TellOnlyRef[Msg]

	actor *actor.Actor[Msg, Resp]

	// targets indexes the watch plan armed for each recipient VTXO.
	targets map[wire.OutPoint]*trackedTarget

	// watches collects the per-ancestor-outpoint refcounted state.
	// Its set-of-targets per outpoint doubles as the reference count
	// so the watcher never has to keep a separate counter + a
	// pkScript map + a target-set map in sync.
	watches ancestorWatches
}

// Ref returns the public watcher actor reference.
func (w *WatcherActor) Ref() actor.ActorRef[Msg, Resp] {
	if w == nil || w.actor == nil {
		return nil
	}

	return w.actor.Ref()
}

// Stop stops the underlying watcher actor.
func (w *WatcherActor) Stop() {
	if w == nil || w.actor == nil {
		return
	}

	w.actor.Stop()
}

// NewWatcherActor creates and starts the recipient fraud watcher actor.
func NewWatcherActor(cfg WatcherConfig) *WatcherActor {
	w := &WatcherActor{
		cfg:     cfg,
		log:     cfg.Log.UnwrapOr(btclog.Disabled),
		targets: make(map[wire.OutPoint]*trackedTarget),
		watches: newAncestorWatches(),
	}

	mailbox := cfg.MailboxSize
	if mailbox <= 0 {
		mailbox = defaultWatcherMailboxSize
	}
	w.actor = actor.NewActor(actor.ActorConfig[Msg, Resp]{
		ID:          ServiceKeyName,
		Behavior:    w,
		MailboxSize: mailbox,
	})
	w.selfRef = w.actor.TellRef()
	w.actor.Start()

	return w
}

// Receive processes one watcher actor message.
func (w *WatcherActor) Receive(ctx context.Context, msg Msg) fn.Result[Resp] {
	var (
		resp Resp
		err  error
	)

	switch m := msg.(type) {
	case *TrackVTXOsRequest:
		resp, err = w.handleTrackVTXOs(ctx, m)

	case *UntrackRequest:
		resp, err = w.handleUntrack(ctx, m)

	case *SpendObservedMsg:
		resp, err = w.handleSpendObserved(ctx, m)

	default:
		err = fmt.Errorf("unknown fraud watcher message: %T", msg)
	}

	if err != nil {
		return fn.Err[Resp](err)
	}

	return fn.Ok[Resp](resp)
}

// OnStop releases every active spend watch. A failure on one outpoint
// must not abandon the rest, so each failure is logged and aggregated
// into the returned joined error.
func (w *WatcherActor) OnStop(ctx context.Context) error {
	var errs error
	for _, outpoint := range w.watches.outpoints() {
		point, ok := w.watches.pointAt(outpoint)
		if !ok {
			continue
		}
		if err := w.unregisterSpendWatchPoint(ctx, point); err != nil {
			w.log.WarnS(
				ctx,
				"Failed to unregister spend watch on stop",
				err,
				slog.String("outpoint", outpoint.String()),
			)
			errs = joinTrackError(errs, err)
		}
	}

	return errs
}

// handleTrackVTXOs admits a batch of OOR VTXO descriptors and arms their
// ancestor spend watches. Per-descriptor failures are aggregated rather
// than short-circuiting the batch so one malformed descriptor cannot
// disarm fraud defenses for the rest. A summary line is emitted so
// daemon-startup callers (which use Tell and drop the returned error)
// still see partial failures in the operator logs.
func (w *WatcherActor) handleTrackVTXOs(ctx context.Context,
	req *TrackVTXOsRequest) (Resp, error) {

	if req == nil {
		return nil, fmt.Errorf("%w: track request is nil",
			ErrWatchUnavailable)
	}

	var (
		tracked  int
		failures int
		errs     error
	)
	for _, desc := range req.VTXOs {
		if !shouldTrackDescriptor(desc) {
			continue
		}

		created, err := w.trackDescriptor(ctx, desc)
		if err != nil {
			failures++
			errs = joinTrackError(errs, err)
			continue
		}
		if created {
			tracked++
		}
	}

	w.log.InfoS(ctx, "Recipient fraud track batch completed",
		slog.Int("requested", len(req.VTXOs)),
		slog.Int("tracked", tracked),
		slog.Int("failures", failures),
	)

	if errs != nil {
		return nil, errs
	}

	return &TrackVTXOsResp{Tracked: tracked}, nil
}

// trackDescriptor builds the watch plan for one VTXO descriptor and
// retains the ancestor watches the plan demands. Returns true when the
// descriptor was newly admitted, false when it was already tracked.
// Roll back any successful watch registrations if a later watch fails
// so the watcher never leaks a half-armed target.
func (w *WatcherActor) trackDescriptor(ctx context.Context,
	desc *vtxo.Descriptor) (bool, error) {

	if _, ok := w.targets[desc.Outpoint]; ok {
		return false, nil
	}

	plan, err := BuildWatchPlan(desc)
	if err != nil {
		return false, fmt.Errorf("build fraud watch plan for %s: %w",
			desc.Outpoint, err)
	}

	registered := make([]wire.OutPoint, 0, len(plan.Watches))
	for _, watch := range plan.Watches {
		if err := w.retainWatch(ctx, watch, desc.Outpoint); err != nil {
			for _, outpoint := range registered {
				_ = w.releaseWatch(ctx, outpoint, desc.Outpoint)
			}

			return false, err
		}
		registered = append(registered, watch.Outpoint)
	}

	w.targets[desc.Outpoint] = &trackedTarget{plan: plan}

	return true, nil
}

// handleUntrack drops fraud watcher interest in one target VTXO and
// releases every watch the target's plan had armed. A missing target
// is a successful no-op so callers can issue Untrack idempotently from
// the VTXO terminal-observer path.
//
// Per-watch release failures are aggregated rather than short-
// circuiting the loop: the target has already been removed from
// w.targets when the release loop runs, so an early return would
// strand the surviving watches with a target that no longer exists,
// and a retried Untrack for the same outpoint would see target==nil
// and silently succeed without finishing cleanup. Continuing through
// the loop releases everything we can and surfaces the aggregated
// error to the caller.
func (w *WatcherActor) handleUntrack(ctx context.Context, req *UntrackRequest) (
	Resp, error) {

	if req == nil {
		return nil, fmt.Errorf("untrack request is nil")
	}

	target := w.targets[req.TargetOutpoint]
	if target == nil {
		return &UntrackResp{}, nil
	}

	delete(w.targets, req.TargetOutpoint)
	var errs error
	for _, watch := range target.plan.Watches {
		if err := w.releaseWatch(
			ctx, watch.Outpoint, req.TargetOutpoint,
		); err != nil {

			w.log.WarnS(
				ctx, "Failed to release fraud watch on untrack",
				err,
				slog.String(
					"watch_outpoint",
					watch.Outpoint.String(),
				),
				slog.String(
					"target_outpoint",
					req.TargetOutpoint.String(),
				),
			)
			errs = joinTrackError(errs, err)
		}
	}

	if errs != nil {
		return nil, errs
	}

	return &UntrackResp{Removed: true}, nil
}

// handleSpendObserved fans out a watched-ancestor spend event into one
// unroll EnsureUnroll call per affected target VTXO. Per-target
// failures are aggregated so a single bad target does not block fraud
// defenses for the others sharing the same ancestor.
func (w *WatcherActor) handleSpendObserved(ctx context.Context,
	msg *SpendObservedMsg) (Resp, error) {

	if msg == nil {
		return nil, fmt.Errorf("spend observed message is nil")
	}

	targets := w.watches.targetsOf(msg.Outpoint)
	if len(targets) == 0 {
		return &AckResp{}, nil
	}
	watch, hasWatch := w.watches.pointAt(msg.Outpoint)
	isOperatorSweep := hasWatch && operatorSweepSpend(msg, watch)
	suppressed := make(map[wire.OutPoint]struct{})
	if isOperatorSweep {
		for target := range targets {
			tracked := w.targets[target]
			if tracked == nil ||
				tracked.plan.BatchExpiry > msg.Height {

				continue
			}
			suppressed[target] = struct{}{}
		}
	}

	// A confirmed spend through the exact timeout leaf committed into the
	// watched tree output is the operator's legitimate batch sweep, not
	// recipient fraud. First drive the manager's normal local-height
	// classifier synchronously; only suppress fraud escalation if that
	// durable classification succeeds. This also closes the startup race in
	// which a restored spend watch can fire before the first block epoch.
	if len(suppressed) > 0 {
		if err := w.reconcileExpiry(ctx, msg.Height); err != nil {
			return nil, err
		}
	}

	var errs error
	for target := range targets {
		if _, ok := suppressed[target]; ok {
			continue
		}
		if err := w.ensureUnroll(ctx, target); err != nil {
			errs = joinTrackError(errs, err)
		}
	}

	w.log.DebugS(ctx, "Triggered recipient fraud unroll",
		slog.String("watched_outpoint", msg.Outpoint.String()),
		slog.String("spending_txid", msg.SpendingTxid.String()),
		slog.Int("height", int(msg.Height)),
		slog.Int("targets", len(targets)),
		slog.Int("expiry_sweeps", len(suppressed)),
	)

	if errs != nil {
		return nil, errs
	}

	return &AckResp{}, nil
}

// reconcileExpiry drives the VTXO manager's authoritative height classifier
// before an operator sweep is allowed to bypass fraud escalation.
func (w *WatcherActor) reconcileExpiry(ctx context.Context,
	height int32) error {

	resp, err := w.cfg.VTXOManagerRef.Ask(
		ctx, &vtxo.ReconcileExpiryRequest{Height: height},
	).Await(ctx).Unpack()
	if err != nil {
		return fmt.Errorf("classify operator sweep at height %d: %w",
			height, err)
	}
	if _, ok := resp.(*vtxo.ReconcileExpiryResponse); !ok {
		return fmt.Errorf("unexpected expiry response %T", resp)
	}

	return nil
}

// operatorSweepSpend verifies that a confirmed watched-tree input was spent
// through the exact timeout tapleaf committed in its persisted ancestry. A
// key-path tree materialization or a leaf-output checkpoint spend does not
// match and still triggers the fraud path.
func operatorSweepSpend(msg *SpendObservedMsg, watch WatchPoint) bool {
	if msg == nil || msg.SpendingTx == nil ||
		len(watch.SweepTapscriptRoot) != chainhash.HashSize ||
		!txscript.IsPayToTaproot(watch.PkScript) {
		return false
	}
	if int(msg.SpenderInputIndex) >= len(msg.SpendingTx.TxIn) {
		return false
	}
	txIn := msg.SpendingTx.TxIn[msg.SpenderInputIndex]
	if txIn.PreviousOutPoint != msg.Outpoint {
		return false
	}

	witness := txIn.Witness
	if len(witness) > 0 && len(witness[len(witness)-1]) > 0 &&
		witness[len(witness)-1][0] == txscript.TaprootAnnexTag {

		witness = witness[:len(witness)-1]
	}
	if len(witness) < 3 {
		return false
	}
	revealedScript := witness[len(witness)-2]
	controlBlock, err := txscript.ParseControlBlock(
		witness[len(witness)-1],
	)
	if err != nil {
		return false
	}
	leafHash := txscript.NewTapLeaf(
		controlBlock.LeafVersion, revealedScript,
	).TapHash()
	if !bytes.Equal(leafHash[:], watch.SweepTapscriptRoot) {
		return false
	}

	version, witnessProgram, err := txscript.ExtractWitnessProgramInfo(
		watch.PkScript,
	)
	if err != nil || version != 1 || len(witnessProgram) != 32 {
		return false
	}

	return txscript.VerifyTaprootLeafCommitment(
		controlBlock, witnessProgram, revealedScript,
	) == nil
}

// retainWatch records target as interested in watch.Outpoint. On the
// first reference for the outpoint it also registers a chainsource
// spend watch; if that registration fails the collection stays
// unchanged so a retry sees the same first-reference state.
// Subsequent retains by other targets only grow the target set.
func (w *WatcherActor) retainWatch(ctx context.Context, watch WatchPoint,
	target wire.OutPoint) error {

	if w.watches.firstRefFor(watch.Outpoint) {
		if err := w.registerSpendWatch(ctx, watch); err != nil {
			return err
		}
	}

	w.watches.addTarget(watch, target)

	return nil
}

// releaseWatch drops target's interest in outpoint. When the target
// set drains to empty the watcher unregisters the chainsource spend
// watch using the stored WatchPoint.
func (w *WatcherActor) releaseWatch(ctx context.Context,
	outpoint, target wire.OutPoint) error {

	point, emptied := w.watches.dropTarget(outpoint, target)
	if !emptied {
		return nil
	}

	return w.unregisterSpendWatchPoint(ctx, point)
}

// registerSpendWatch installs a chainsource spend watch for one
// ancestor outpoint. Watch events are mapped into SpendObservedMsg and
// delivered back through the watcher's own mailbox.
func (w *WatcherActor) registerSpendWatch(ctx context.Context,
	watch WatchPoint) error {

	notifyRef := chainsource.MapSpendEvent(
		w.selfRef, func(event chainsource.SpendEvent) Msg {
			return &SpendObservedMsg{
				Outpoint:          event.Outpoint,
				SpendingTxid:      event.SpendingTxid,
				SpendingTx:        event.SpendingTx,
				SpenderInputIndex: event.SpenderInputIndex,
				Height:            event.SpendingHeight,
			}
		},
	)

	outpoint := watch.Outpoint
	_, err := w.cfg.ChainSource.Ask(ctx, &chainsource.RegisterSpendRequest{
		CallerID:    ServiceKeyName,
		Outpoint:    &outpoint,
		PkScript:    append([]byte(nil), watch.PkScript...),
		HeightHint:  watch.HeightHint,
		NotifyActor: fn.Some(notifyRef),
	}).Await(ctx).Unpack()
	if err != nil {
		return fmt.Errorf("register fraud spend watch %s: %w",
			watch.Outpoint, err)
	}

	return nil
}

// unregisterSpendWatchPoint removes the chainsource spend watch
// installed by registerSpendWatch. The caller supplies the WatchPoint
// (rather than looking it up by outpoint) so the helper works equally
// well from the per-release path — where dropTarget already returned
// the point — and from the shutdown / diagnostic path that reads the
// collection separately.
func (w *WatcherActor) unregisterSpendWatchPoint(ctx context.Context,
	watch WatchPoint) error {

	outpoint := watch.Outpoint
	req := &chainsource.UnregisterSpendRequest{
		CallerID: ServiceKeyName,
		Outpoint: &outpoint,
		PkScript: append([]byte(nil), watch.PkScript...),
	}
	_, err := w.cfg.ChainSource.Ask(ctx, req).Await(ctx).Unpack()
	if err != nil {
		return fmt.Errorf("unregister fraud spend watch %s: %w",
			outpoint, err)
	}

	return nil
}

// ensureUnroll asks the VTXO manager to force one target VTXO into unilateral
// exit under TriggerFraudSpend. The manager owns the state transition and, via
// its chain-resolver seam, starts (or reuses) the durable unroll job. A
// declined transition (the coin is already terminal, or the wallet no longer
// tracks it) is logged rather than surfaced as a hard error: the fraud watch
// has done all it can, and neither case is one the watcher can drive forward,
// so failing would only wedge escalation for the other targets sharing the
// ancestor.
func (w *WatcherActor) ensureUnroll(ctx context.Context,
	target wire.OutPoint) error {

	resp, err := w.cfg.VTXOManagerRef.Ask(ctx, &actormsg.ForceUnrollRequest{
		Outpoint: target,
		Reason:   "recipient fraud spend",
		Trigger:  actormsg.UnrollTriggerFraudSpend,
	}).Await(ctx).Unpack()
	if err != nil {
		return fmt.Errorf("ensure fraud unroll for %s: %w", target, err)
	}

	forceResp, ok := resp.(*actormsg.ForceUnrollResponse)
	if !ok {
		return fmt.Errorf("unexpected force-unroll response %T for %s",
			resp, target)
	}

	if !forceResp.Accepted {
		w.log.WarnS(ctx, "VTXO manager declined fraud unroll",
			nil,
			slog.String("outpoint", target.String()),
			slog.String("reason", forceResp.Reason),
		)
	}

	return nil
}
