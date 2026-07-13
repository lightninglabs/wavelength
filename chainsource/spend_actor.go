package chainsource

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/build"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

// SpendActorConfig holds configuration for SpendActor.
type SpendActorConfig struct {
	// Backend is the blockchain backend used to monitor spends.
	Backend ChainBackend

	// Log is an optional logger for this actor instance. If None, the actor
	// falls back to extracting a logger from context via LoggerFromContext,
	// or uses btclog.Disabled if no logger is found.
	Log fn.Option[btclog.Logger]

	// FinalityDepth is the number of confirmations past the first
	// observed Spend event that the actor uses to synthesize a Done
	// signal when the backend cannot deliver one. See the matching
	// field on ConfActorConfig for the rationale; the same constraint
	// applies on the spend watch — lnd's chainntnfs.SpendEvent.Done
	// does not survive the lndclient gRPC transport, so consumers
	// that gate eviction on Done would otherwise leak per-spend state.
	FinalityDepth uint32
}

// WithLogger returns a new config with the given logger set.
func (c SpendActorConfig) WithLogger(log btclog.Logger) SpendActorConfig {
	c.Log = fn.Some(log)

	return c
}

// SpendActor is a single-subscription actor that monitors outpoint spends and
// delivers events when outputs are consumed by confirmed transactions. Each
// instance serves exactly one subscription.
//
// The actor supports dual-mode operation: Future mode for blocking await
// (exits after first event), and Actor mode for asynchronous event delivery
// (continues monitoring for re-orgs).
type SpendActor struct {
	// cfg holds all actor configuration including backend and optional
	// logger.
	cfg SpendActorConfig

	// outpoint is the output being monitored.
	outpoint *wire.OutPoint

	// pkScript is the public key script being monitored.
	pkScript []byte

	// heightHint is the earliest block that could contain a spending tx.
	heightHint uint32

	// promise is used in Future mode to complete the future when the spend
	// is detected.
	promise fn.Option[actor.Promise[SpendEvent]]

	// notifyActor is used in Actor mode to send events. None in Future
	// mode.
	notifyActor fn.Option[actor.TellOnlyRef[SpendEvent]]

	// notifyReorged receives spend reorg events in Actor mode.
	notifyReorged fn.Option[actor.TellOnlyRef[SpendReorgedEvent]]

	// notifyDone receives spend finality events in Actor mode.
	notifyDone fn.Option[actor.TellOnlyRef[SpendDoneEvent]]

	// registration is the backend registration for this watch.
	registration *SpendRegistration

	// blockReg is the block-epoch subscription used by height-based
	// finality synthesis. Allocated lazily after the first Spend
	// event when FinalityDepth > 0, torn down when the actor exits.
	blockReg *BlockRegistration

	// spendHeight records the block height the most recent Spend
	// event arrived at. Zero means there is no active spend to count
	// from (either we have not yet seen one, or the last one was
	// reorged out).
	spendHeight int32

	// ctx is the actor's internal context for cancellation, created from
	// context.Background() to ensure it outlives any request context.
	//nolint:containedctx
	ctx context.Context

	// cancel cancels the actor's context.
	cancel context.CancelFunc

	// wg tracks background goroutines for graceful shutdown.
	wg sync.WaitGroup
}

// logger returns the configured logger or falls back to extracting from
// context. If no logger is found in either location, returns btclog.Disabled.
func (a *SpendActor) logger(ctx context.Context) btclog.Logger {
	return a.cfg.Log.UnwrapOr(build.LoggerFromContext(ctx))
}

// NewSpendActor creates a new SpendActor instance with the given configuration.
// The config must include Backend; use WithLogger() to inject a specific
// logger.
func NewSpendActor(cfg SpendActorConfig) *SpendActor {
	// Use background context for internal cancellation since the actor
	// needs to outlive any request context. Logger is passed via config.
	ctx, cancel := context.WithCancel(context.Background())

	return &SpendActor{
		cfg:    cfg,
		ctx:    ctx,
		cancel: cancel,
	}
}

// Receive processes incoming messages for the SpendActor.
func (a *SpendActor) Receive(actorCtx context.Context,
	msg SpendMsg) fn.Result[SpendResp] {

	switch m := msg.(type) {
	case *RegisterSpendRequest:
		return a.handleRegisterSpend(actorCtx, m)

	default:
		return fn.Err[SpendResp](
			fmt.Errorf("unknown message type: %T", msg),
		)
	}
}

// handleRegisterSpend processes a spend registration request. It configures
// the actor and starts monitoring.
func (a *SpendActor) handleRegisterSpend(actorCtx context.Context,
	req *RegisterSpendRequest) fn.Result[SpendResp] {

	// Each SpendActor instance serves exactly one subscription. Reject
	// duplicate registrations.
	if a.registration != nil {
		return fn.Err[SpendResp](
			fmt.Errorf("actor already has an active subscription"),
		)
	}

	// Validate request parameters.
	if req.Outpoint == nil && len(req.PkScript) == 0 {
		return fn.Err[SpendResp](
			fmt.Errorf("either outpoint or pkScript must be " +
				"provided"),
		)
	}
	if req.NotifyActor.IsNone() &&
		(req.NotifyReorged.IsSome() || req.NotifyDone.IsSome()) {
		return fn.Err[SpendResp](
			fmt.Errorf("spend reorg/done notifications require " +
				"actor-mode NotifyActor"),
		)
	}

	// Configure the actor with request parameters.
	a.outpoint = req.Outpoint
	a.pkScript = req.PkScript
	a.heightHint = req.HeightHint
	a.notifyActor = req.NotifyActor
	a.notifyReorged = req.NotifyReorged
	a.notifyDone = req.NotifyDone

	// Create promise for Future mode.
	var promise fn.Option[actor.Promise[SpendEvent]]
	if req.NotifyActor.IsNone() {
		// Future mode: create a promise.
		promise = fn.Some(actor.NewPromise[SpendEvent]())
	} else {
		// Actor mode: no promise needed.
		promise = fn.None[actor.Promise[SpendEvent]]()
	}
	a.promise = promise

	// Register with the backend to receive spend notifications. We do this
	// before starting the goroutine so we can return an error to the
	// caller if registration fails. The bounded timeout mirrors
	// ConfActor.handleRegisterConf: a backend (LND) that is slow under
	// heavy block processing load must not pin the parent Receive call
	// indefinitely, since that would back-pressure the chainsource
	// factory actor onto every other in-flight registration.
	regCtx, regCancel := context.WithTimeout(a.ctx, 10*time.Second)
	defer regCancel()

	//nolint:contextcheck // actor root context owns registration lifetime
	registration, err := a.cfg.Backend.RegisterSpend(
		regCtx, a.outpoint, a.pkScript, a.heightHint,
	)
	if err != nil {
		return fn.Err[SpendResp](
			fmt.Errorf("failed to register for spends: %w", err),
		)
	}
	a.registration = registration

	// Start monitoring in background.
	a.wg.Add(1)
	go a.monitorSpend()

	// Build response.
	resp := &RegisterSpendResponse{}

	// Add Future for blocking mode.
	promise.WhenSome(func(p actor.Promise[SpendEvent]) {
		resp.Future = p.Future()
	})

	return fn.Ok[SpendResp](resp)
}

// monitorSpend runs in a background goroutine and waits for spend events from
// the backend. It continues monitoring to handle re-orgs until cancelled or
// (for Future mode) the first event is delivered.
func (a *SpendActor) monitorSpend() {
	defer a.wg.Done()
	defer a.cancel()

	// Clean up registration when done.
	defer func() {
		if a.registration != nil {
			a.registration.Cancel()
		}
		if a.blockReg != nil {
			a.blockReg.Cancel()
		}
	}()

	log := a.logger(a.ctx)

	// Monitor for spends indefinitely until cancelled or shutdown.
	// This allows us to catch re-org events where a spend is replaced.
	var lastEvent *SpendEvent

	// reorgAware reports whether the caller opted into the multi-shot
	// reorg lifecycle (at least one of NotifyReorged/NotifyDone). When
	// false the watch is single-shot for backwards compatibility: the
	// actor exits after the first spend, mirroring ConfActor. Without
	// this gate a plain actor-mode spend watch would run forever and, with
	// FinalityDepth > 0, arm a block subscription it never asked for.
	reorgAware := a.notifyReorged.IsSome() || a.notifyDone.IsSome()

	// lastSeq is the highest backend forwarder sequence applied so far.
	// Spend and Reorged signals arrive on separate channels and a select
	// cannot order two ready channels, so we order them by the shared
	// sequence instead: an event whose Seq does not exceed lastSeq lost a
	// cross-channel race to a newer signal and is discarded. This makes
	// the actor's view correct regardless of delivery interleaving. Seq 0
	// means the backend does not stamp sequences (it never reorgs); those
	// events are always applied.
	var lastSeq uint64

	// blockEpochs is rebound when height-based finality synthesis
	// arms a block subscription. Until then a nil channel keeps the
	// select arm parked.
	var blockEpochs <-chan *BlockEpoch

	// blockRegCh hands a finality block subscription from the off-loop
	// arming goroutine back to this loop; arming guards against launching
	// more than one armer at a time. See armFinalityAsync for why arming
	// runs off the select loop.
	blockRegCh := make(chan finalityArmResult)
	var arming bool

	for {
		select {
		case spend, ok := <-a.registration.Spend:
			if !ok || spend == nil {
				a.failSpend(
					errors.New("spend subscription closed"),
				)

				return
			}

			// Discard a spend that lost a cross-channel race to a
			// newer reorg: an event whose sequence does not exceed
			// the highest applied is stale.
			if spend.Seq != 0 && spend.Seq <= lastSeq {
				continue
			}
			if spend.Seq > lastSeq {
				lastSeq = spend.Seq
			}

			event, err := buildSpendEvent(spend, a)
			if err != nil {
				a.failSpend(err)

				return
			}

			// Deliver the event.
			a.deliverSpend(event)
			lastEvent = &event
			a.spendHeight = event.SpendingHeight

			// Exit after the first event in Future mode, and in
			// actor mode that did not opt into the reorg lifecycle
			// (single-shot backwards-compatible contract). Only a
			// reorg-aware actor watch keeps monitoring.
			if !reorgAware || a.promise.IsSome() {
				return
			}

			// Arm height-based finality synthesis on the first
			// spend if requested, off the select loop so the
			// bounded RegisterBlocks retries cannot stall delivery
			// of Reorged/Done/ctx.Done on this watch. A nil
			// blockEpochs channel keeps the synthesis arm parked
			// until the registration is handed back on blockRegCh.
			if a.cfg.FinalityDepth > 0 && a.blockReg == nil &&
				!arming {

				arming = true
				a.armFinalityAsync(blockRegCh, log)
			}

		case armed := <-blockRegCh:
			// Finality arming completed. Clear the flag; a nil reg
			// only happens when the watch context was cancelled
			// (arming otherwise retries until it succeeds).
			arming = false
			if armed.reg == nil {
				continue
			}
			a.blockReg = armed.reg
			blockEpochs = armed.reg.Epochs

			// The block-epoch subscription only delivers FUTURE
			// epochs, but the spend that armed it may already be
			// buried past FinalityDepth (it confirmed several
			// blocks ago, or we re-armed after a restart). Use the
			// tip observed at arm time to synthesize Done at once
			// rather than hang until a fresh block is mined. The
			// spendHeight==0 / FinalityDepth==0 guards mirror the
			// epoch handler below.
			if a.spendHeight == 0 || a.cfg.FinalityDepth == 0 {
				continue
			}
			if armed.height-a.spendHeight+1 <
				int32(a.cfg.FinalityDepth) {

				continue
			}

			log.InfoS(a.ctx, "Synthesizing spend done on arm from "+
				"height-based safety depth",
				"spend_height", a.spendHeight,
				"current_height", armed.height,
				"finality_depth", int(a.cfg.FinalityDepth),
			)
			a.deliverSpendDone(lastEvent)

			return

		case seq, ok := <-a.registration.Reorged:
			if !ok {
				a.registration.Reorged = nil
				continue
			}

			// Discard a stale reorg that lost a cross-channel race
			// to a newer spend.
			if seq != 0 && seq <= lastSeq {
				continue
			}
			if seq > lastSeq {
				lastSeq = seq
			}

			a.deliverSpendReorged(lastEvent)

			// The previous spend is no longer on the canonical
			// chain. Clear the cached event so a later Done cannot
			// report the reorged-out outpoint, and reset the depth
			// counter so the next re-spend starts a fresh window.
			lastEvent = nil
			a.spendHeight = 0

		case _, ok := <-a.registration.Done:
			if !ok {
				a.registration.Done = nil
				continue
			}

			a.deliverSpendDone(lastEvent)

			return

		case epoch, ok := <-blockEpochs:
			if !ok || epoch == nil {
				blockEpochs = nil
				continue
			}

			// Coalesce any epochs already queued behind this one
			// and evaluate finality against the most recent height
			// only. With rapid-fire blocks the channel can hold
			// several epochs at once; processing them one per loop
			// iteration would re-check the same monotonic Done
			// condition repeatedly and risk synthesizing against a
			// stale height. If the channel closed during the drain,
			// park it so we stop selecting on it.
			var closed bool
			epoch, closed = drainToLatestEpoch(blockEpochs, epoch)
			if closed {
				blockEpochs = nil
			}

			// The spendHeight==0 guard is load-bearing: a reorg
			// resets spendHeight to 0 (the Reorged arm above), so
			// a fresh epoch arriving before the re-spend would
			// otherwise compute depth against a zero base and
			// could synthesize Done prematurely. While
			// spendHeight==0 there is no active spend to count
			// from, so the depth comparison is meaningless.
			// FinalityDepth==0 disables synthesis entirely.
			if a.spendHeight == 0 ||
				a.cfg.FinalityDepth == 0 {

				continue
			}

			depth := epoch.Height - a.spendHeight + 1
			if depth < int32(a.cfg.FinalityDepth) {
				continue
			}

			log.InfoS(a.ctx, "Synthesizing spend done from "+
				"height-based safety depth",
				"spend_height", a.spendHeight,
				"current_height", epoch.Height,
				"finality_depth", int(a.cfg.FinalityDepth),
			)
			a.deliverSpendDone(lastEvent)

			return

		case <-a.ctx.Done():
			// Actor was cancelled.
			a.failSpend(a.ctx.Err())

			return
		}
	}
}

// armFinalityAsync registers a block-epoch subscription for height-based
// finality synthesis off the actor's select loop. registerBlocksForFinality
// retries with a bounded backoff that can run for tens of seconds; doing it
// inline would block delivery of Reorged/Done/ctx.Done on this watch for the
// whole window. The registration (or nil on failure) is handed back on regCh,
// or cancelled if the actor exits before the loop reads it. The goroutine is
// tracked by the actor's wait group so Stop drains it.
func (a *SpendActor) armFinalityAsync(regCh chan<- finalityArmResult,
	log btclog.Logger) {

	a.wg.Go(func() {
		reg, err := registerBlocksForFinality(a.ctx, a.cfg.Backend, log)
		if err != nil {
			log.WarnS(a.ctx, "Giving up on height-based finality "+
				"synthesis; spend sub-actor will rely on "+
				"backend Done", err)
			reg = nil
		}

		// Capture the tip at arm time so the loop can finalize
		// immediately when the arming spend is already buried past
		// FinalityDepth (the block-epoch sub only delivers future
		// epochs). A read failure is non-fatal: height stays zero and
		// the loop falls back to waiting for the next epoch.
		var height int32
		if reg != nil {
			h, _, hErr := a.cfg.Backend.BestBlock(a.ctx)
			if hErr != nil {
				log.WarnS(a.ctx, "Failed to read best height "+
					"for on-arm finality check; will wait "+
					"for next epoch", hErr)
			} else {
				height = h
			}
		}

		select {
		case regCh <- finalityArmResult{reg: reg, height: height}:
		case <-a.ctx.Done():
			if reg != nil {
				reg.Cancel()
			}
		}
	})
}

// deliverSpend delivers a spend event to the subscriber. In Future mode, it
// completes the promise. In Actor mode, it sends to the registered actor.
func (a *SpendActor) deliverSpend(event SpendEvent) {
	a.promise.WhenSome(func(p actor.Promise[SpendEvent]) {
		// Future mode: complete the promise.
		p.Complete(fn.Ok(event))
	})

	a.notifyActor.WhenSome(func(ref actor.TellOnlyRef[SpendEvent]) {
		log := a.logger(a.ctx)

		// Actor mode: send to the registered actor.
		if err := ref.Tell(a.ctx, event); err != nil {
			log.WarnS(a.ctx, "Failed to deliver spend event", err)
		}
	})
}

// deliverSpendReorged delivers a spend reorg event to actor-mode
// subscribers. The correlation Outpoint is the registration's configured
// outpoint when set, since that is the identifier the caller asked us to
// watch; pkScript-only watches fall back to the outpoint carried on the
// most recent positive SpendEvent.
func (a *SpendActor) deliverSpendReorged(lastEvent *SpendEvent) {
	var event SpendReorgedEvent
	switch {
	case a.outpoint != nil:
		event.Outpoint = *a.outpoint

	case lastEvent != nil:
		event.Outpoint = lastEvent.Outpoint
	}

	a.notifyReorged.WhenSome(
		func(ref actor.TellOnlyRef[SpendReorgedEvent]) {
			log := a.logger(a.ctx)
			if err := ref.Tell(a.ctx, event); err != nil {
				log.WarnS(
					a.ctx,
					"Failed to deliver spend reorg",
					err,
				)
			}
		},
	)
}

// deliverSpendDone delivers a spend finality event to actor-mode subscribers.
// Outpoint follows the same precedence as deliverSpendReorged.
func (a *SpendActor) deliverSpendDone(lastEvent *SpendEvent) {
	var event SpendDoneEvent
	switch {
	case a.outpoint != nil:
		event.Outpoint = *a.outpoint

	case lastEvent != nil:
		event.Outpoint = lastEvent.Outpoint
	}

	a.notifyDone.WhenSome(func(ref actor.TellOnlyRef[SpendDoneEvent]) {
		log := a.logger(a.ctx)
		if err := ref.Tell(a.ctx, event); err != nil {
			log.WarnS(a.ctx, "Failed to deliver spend done", err)
		}
	})
}

// failSpend completes the promise with an error (Future mode) or does nothing
// (Actor mode - errors are not delivered in async mode).
func (a *SpendActor) failSpend(err error) {
	a.promise.WhenSome(func(p actor.Promise[SpendEvent]) {
		p.Complete(fn.Err[SpendEvent](err))
	})
}

// Stop gracefully shuts down the SpendActor. It cancels the context and waits
// for the monitoring goroutine to complete.
func (a *SpendActor) Stop() {
	// Cancel the context to signal shutdown.
	a.cancel()

	// Wait for the monitoring goroutine to complete.
	a.wg.Wait()
}

// OnStop implements actor.Stoppable for proper cleanup when stopped via actor
// system. This is called after the actor's message loop exits.
func (a *SpendActor) OnStop(ctx context.Context) error {
	// Cancel internal context to signal background goroutine.
	a.cancel()

	// Wait for goroutine with timeout from cleanup context.
	done := make(chan struct{})
	go func() {
		a.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil

	case <-ctx.Done():
		return ctx.Err()
	}
}

// buildSpendEvent converts the backend SpendDetail into a SpendEvent,
// filling in missing fields where possible.
func buildSpendEvent(spend *SpendDetail,
	watch *SpendActor) (SpendEvent, error) {

	spenderHash, err := spendTxHash(spend)
	if err != nil {
		return SpendEvent{}, err
	}

	event := SpendEvent{
		SpendingTxid:      spenderHash,
		SpendingTx:        spend.SpendingTx,
		SpenderInputIndex: spend.SpenderInputIndex,
		SpendingHeight:    spend.SpendingHeight,
	}

	if spend.SpentOutPoint != nil {
		event.Outpoint = *spend.SpentOutPoint
	} else if watch.outpoint != nil {
		event.Outpoint = *watch.outpoint
	}

	return event, nil
}

// spendTxHash determines the spending transaction hash, falling back to the
// transaction contents when needed.
func spendTxHash(spend *SpendDetail) (chainhash.Hash, error) {
	switch {
	case spend.SpenderTxHash != nil:
		return *spend.SpenderTxHash, nil

	case spend.SpendingTx != nil:
		return spend.SpendingTx.TxHash(), nil

	default:
		return chainhash.Hash{}, fmt.Errorf("spend event missing " +
			"transaction hash")
	}
}
