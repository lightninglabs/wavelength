package chainsource

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/build"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

// ConfActorConfig holds configuration for ConfActor.
type ConfActorConfig struct {
	// Backend is the blockchain backend used to monitor confirmations.
	Backend ChainBackend

	// Log is an optional logger for this actor instance. If None, the actor
	// falls back to extracting a logger from context via LoggerFromContext,
	// or uses btclog.Disabled if no logger is found.
	Log fn.Option[btclog.Logger]

	// FinalityDepth is the number of confirmations past the first
	// observed Confirmed event that the actor uses to synthesize a Done
	// signal when the backend cannot deliver one. Zero disables
	// height-based finality synthesis entirely; in that case the actor
	// only fires ConfDoneEvent when the backend's own Done channel
	// fires (e.g. an in-process lnd notifier). Non-zero values close
	// the lndclient transport gap, where the gRPC layer does not
	// surface lnd's internal "past reorg-safety depth" signal.
	//
	// The depth is counted inclusively (a tx confirmed at height H is
	// at depth 1; height-based finality fires once the actor observes
	// a block at H + FinalityDepth - 1). The conventional choice is 6.
	FinalityDepth uint32
}

// WithLogger returns a new config with the given logger set.
func (c ConfActorConfig) WithLogger(log btclog.Logger) ConfActorConfig {
	c.Log = fn.Some(log)

	return c
}

// ConfActor is a single-subscription actor that monitors transaction
// confirmations and delivers an event when the transaction reaches its target
// confirmation count. Each instance serves exactly one subscription.
//
// The actor supports dual-mode operation: Future mode for blocking await, and
// Actor mode for asynchronous event delivery. The actor exits after delivering
// the confirmation event.
type ConfActor struct {
	// cfg holds all actor configuration including backend and optional
	// logger.
	cfg ConfActorConfig

	// txid is the transaction ID being monitored.
	txid *chainhash.Hash

	// pkScript is the public key script being monitored.
	pkScript []byte

	// targetConfs is the number of confirmations to wait for.
	targetConfs uint32

	// heightHint is the earliest block that could contain the transaction.
	heightHint uint32

	// includeBlock indicates whether to include the full block in the
	// confirmation event. This is needed for constructing merkle proofs.
	includeBlock bool

	// promise is used in Future mode to complete the future when the
	// confirmation is detected.
	promise fn.Option[actor.Promise[ConfirmationEvent]]

	// notifyActor is used in Actor mode to send events. None in Future
	// mode.
	notifyActor fn.Option[actor.TellOnlyRef[ConfirmationEvent]]

	// notifyReorged receives negative confirmation events in Actor mode.
	notifyReorged fn.Option[actor.TellOnlyRef[ConfReorgedEvent]]

	// notifyDone receives finality events in Actor mode.
	notifyDone fn.Option[actor.TellOnlyRef[ConfDoneEvent]]

	// registration is the backend registration for this watch.
	registration *ConfRegistration

	// blockReg is the block-epoch subscription used by height-based
	// finality synthesis. Allocated lazily after the first Confirmed
	// event when FinalityDepth > 0 and the registration is reorg-aware,
	// torn down when the actor exits.
	blockReg *BlockRegistration

	// confirmHeight records the block height the most recent
	// Confirmed event arrived at. Used by the height-based finality
	// synthesizer to compute current depth. Zero means there is no
	// active confirmation to count from (either we have not yet seen
	// one, or the last one was reorged out).
	confirmHeight int32

	// ctx is the actor's internal context for cancellation, created from
	// context.Background() to ensure it outlives any request context.
	//nolint:containedctx
	ctx context.Context

	// cancel cancels the actor's context.
	cancel context.CancelFunc

	// wg tracks background goroutines for graceful shutdown.
	wg sync.WaitGroup
}

// NewConfActor creates a new ConfActor instance with the given configuration.
// The config must include Backend; use WithLogger() to inject a specific
// logger.
func NewConfActor(cfg ConfActorConfig) *ConfActor {
	// Use background context for internal cancellation since the actor
	// needs to outlive any request context. Logger is passed via config.
	ctx, cancel := context.WithCancel(context.Background())

	return &ConfActor{
		cfg:    cfg,
		ctx:    ctx,
		cancel: cancel,
	}
}

// logger returns the configured logger or falls back to extracting from
// context. If no logger is found in either location, returns btclog.Disabled.
func (a *ConfActor) logger(ctx context.Context) btclog.Logger {
	return a.cfg.Log.UnwrapOr(build.LoggerFromContext(ctx))
}

// Receive processes incoming messages for the ConfActor.
func (a *ConfActor) Receive(actorCtx context.Context,
	msg ConfMsg) fn.Result[ConfResp] {

	switch m := msg.(type) {
	case *RegisterConfRequest:
		return a.handleRegisterConf(actorCtx, m)

	default:
		return fn.Err[ConfResp](
			fmt.Errorf("unknown message type: %T", msg),
		)
	}
}

// handleRegisterConf processes a confirmation registration request. It
// configures the actor and starts monitoring.
func (a *ConfActor) handleRegisterConf(actorCtx context.Context,
	req *RegisterConfRequest) fn.Result[ConfResp] {

	// Each ConfActor instance serves exactly one subscription. Reject
	// duplicate registrations.
	if a.registration != nil {
		return fn.Err[ConfResp](
			fmt.Errorf("actor already has an active subscription"),
		)
	}

	// Do some basic validation of the request parameters.
	if req.Txid == nil && len(req.PkScript) == 0 {
		return fn.Err[ConfResp](
			fmt.Errorf("either txid or pkScript must be provided"),
		)
	}
	if req.TargetConfs == 0 {
		return fn.Err[ConfResp](
			fmt.Errorf("target confirmations must be greater " +
				"than zero"),
		)
	}
	if req.NotifyActor.IsNone() &&
		(req.NotifyReorged.IsSome() || req.NotifyDone.IsSome()) {
		return fn.Err[ConfResp](
			fmt.Errorf("confirmation reorg/done notifications " +
				"require actor-mode NotifyActor"),
		)
	}

	a.txid = req.Txid
	a.pkScript = req.PkScript
	a.targetConfs = req.TargetConfs
	a.heightHint = req.HeightHint
	a.includeBlock = req.IncludeBlock
	a.notifyActor = req.NotifyActor
	a.notifyReorged = req.NotifyReorged
	a.notifyDone = req.NotifyDone

	// We're either in future or iterator mode, set the promise
	// accordingly.
	var promise fn.Option[actor.Promise[ConfirmationEvent]]
	if req.NotifyActor.IsNone() {
		promise = fn.Some(actor.NewPromise[ConfirmationEvent]())
	} else {
		promise = fn.None[actor.Promise[ConfirmationEvent]]()
	}
	a.promise = promise

	// Register with the backend to receive confirmation notifications.
	// Use a timeout to prevent hanging if the backend (LND) is slow
	// to respond under heavy block processing load.
	regCtx, regCancel := context.WithTimeout(a.ctx, 10*time.Second)
	defer regCancel()

	//nolint:contextcheck // actor root context owns registration lifetime
	registration, err := a.cfg.Backend.RegisterConf(
		regCtx, a.txid, a.pkScript, a.targetConfs, a.heightHint,
		a.includeBlock,
	)
	if err != nil {
		return fn.Err[ConfResp](
			fmt.Errorf("failed to register for confirmations: %w",
				err),
		)
	}
	a.registration = registration

	// Now we'll kick off our monitoring goroutine which handles the
	// iteration.
	a.wg.Add(1)
	go a.monitorConfirmation()

	resp := &RegisterConfResponse{}

	// If we're in blocking/future mode, then add the Future to the
	// response.
	promise.WhenSome(func(p actor.Promise[ConfirmationEvent]) {
		resp.Future = p.Future()
	})

	return fn.Ok[ConfResp](resp)
}

// monitorConfirmation runs in a background goroutine and waits for the target
// confirmation count to be reached. Legacy watches exit after confirmation,
// while reorg-aware actor-mode watches remain alive for reorg and done events.
func (a *ConfActor) monitorConfirmation() {
	defer a.wg.Done()
	defer a.cancel()

	log := a.logger(a.ctx)
	log.InfoS(a.ctx, "ConfActor monitoring started",
		"target_confs", a.targetConfs,
		"height_hint", a.heightHint,
	)

	// Clean up registration when done.
	defer func() {
		log.InfoS(a.ctx, "ConfActor monitoring stopped")
		if a.registration != nil {
			a.registration.Cancel()
		}
		if a.blockReg != nil {
			a.blockReg.Cancel()
		}
	}()

	var lastEvent *ConfirmationEvent
	var doneOrder PositiveDoneOrder
	reorgAware := a.notifyReorged.IsSome() || a.notifyDone.IsSome()

	// lastSeq is the highest backend forwarder sequence applied so far.
	// Confirmed and Reorged signals arrive on separate channels and a
	// select cannot order two ready channels, so we order them by the
	// shared sequence instead: an event whose Seq does not exceed lastSeq
	// lost a cross-channel race to a newer signal and is discarded. This
	// makes the actor's view correct regardless of delivery interleaving
	// — both reorg-then-reconfirm and confirm-then-reorg resolve to the
	// highest-sequence outcome. Seq 0 means the backend does not stamp
	// sequences (it never reorgs); those events are always applied.
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
		case confDetails, ok := <-a.registration.Confirmed:
			if !ok || confDetails == nil {
				log.WarnS(
					a.ctx,
					"Confirmation subscription closed",
					fmt.Errorf("channel closed or nil "+
						"details"),
				)
				a.failConfirmation(
					fmt.Errorf("confirmation " +
						"subscription closed"),
				)

				return
			}

			// Discard a confirmation that lost a cross-channel race
			// to a newer reorg: an event whose sequence does not
			// exceed the highest applied is stale.
			if confDetails.Seq != 0 && confDetails.Seq <= lastSeq {
				continue
			}
			if confDetails.Seq > lastSeq {
				lastSeq = confDetails.Seq
			}

			log.InfoS(a.ctx, "Received confirmation from backend",
				"block_height", confDetails.BlockHeight,
				"block_hash", confDetails.BlockHash,
			)

			event, err := buildConfirmationEvent(confDetails, a)
			if err != nil {
				log.WarnS(
					a.ctx,
					"Failed to build confirmation event",
					err,
				)
				a.failConfirmation(err)

				return
			}

			log.InfoS(a.ctx, "Delivering confirmation event",
				"txid", event.Txid,
				"block_height", event.BlockHeight,
			)
			a.deliverConfirmation(event)
			lastEvent = &event
			a.confirmHeight = event.BlockHeight
			if !reorgAware || a.promise.IsSome() {
				return
			}
			if a.applyPendingConfDone(lastEvent, &doneOrder) {
				return
			}

			// Arm height-based finality synthesis on the first
			// confirmation if requested, off the select loop so
			// the bounded RegisterBlocks retries cannot stall
			// delivery of Reorged/Done/ctx.Done on this watch. A
			// nil blockEpochs channel keeps the synthesis arm
			// parked until the registration is handed back on
			// blockRegCh.
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
			// epochs, but the confirmation that armed it may
			// already be buried past FinalityDepth (it confirmed
			// several blocks ago, or we re-armed after a restart).
			// Use the tip observed at arm time to synthesize Done
			// at once rather than hang until a fresh block is
			// mined. The confirmHeight==0 / FinalityDepth==0 guards
			// mirror the epoch handler below.
			if a.confirmHeight == 0 || a.cfg.FinalityDepth == 0 {
				continue
			}
			if armed.height-a.confirmHeight+1 <
				int32(a.cfg.FinalityDepth) {

				continue
			}

			log.InfoS(a.ctx, "Synthesizing confirmation done on arm "+
				"from height-based safety depth",
				"confirm_height", a.confirmHeight,
				"current_height", armed.height,
				"finality_depth", int(a.cfg.FinalityDepth),
			)
			a.deliverConfDone(lastEvent)

			return

		case seq, ok := <-a.registration.Reorged:
			if !ok {
				a.registration.Reorged = nil
				continue
			}

			// Discard a stale reorg that lost a cross-channel race
			// to a newer confirmation.
			if seq != 0 && seq <= lastSeq {
				continue
			}
			if seq > lastSeq {
				lastSeq = seq
			}

			a.applyConfReorg(lastEvent, &doneOrder)
			lastEvent = nil

		case _, ok := <-a.registration.Done:
			if !ok {
				a.registration.Done = nil
				continue
			}
			if a.applyConfDone(lastEvent, &doneOrder) {
				return
			}

		case epoch, ok := <-blockEpochs:
			if !ok || epoch == nil {
				a.clearFinalityRegistration()
				blockEpochs = nil
				if a.confirmHeight != 0 &&
					a.cfg.FinalityDepth > 0 && !arming {

					arming = true
					a.armFinalityAsync(blockRegCh, log)
				}

				continue
			}

			// Coalesce any epochs already queued behind this one
			// and evaluate finality against the most recent
			// height only. With rapid-fire blocks (or a backend
			// that re-delivers historical epochs) the channel can
			// hold several epochs at once; processing them one per
			// loop iteration would re-check the same monotonic
			// Done condition repeatedly and risk synthesizing
			// against a stale height. If the channel closed during
			// the drain, park it so we stop selecting on it.
			var closed bool
			epoch, closed = drainToLatestEpoch(blockEpochs, epoch)
			if closed {
				a.clearFinalityRegistration()
				blockEpochs = nil
			}

			// The confirmHeight==0 guard is load-bearing: a reorg
			// resets confirmHeight to 0 (see the Reorged case
			// above), and a fresh epoch on the new tip would
			// otherwise produce a negative depth (epoch.Height - 0
			// wraps to a large value); the depth comparison is
			// meaningful only after a confirmation arms
			// confirmHeight. FinalityDepth==0 disables synthesis
			// entirely.
			if a.confirmHeight == 0 ||
				a.cfg.FinalityDepth == 0 {

				continue
			}

			depth := epoch.Height - a.confirmHeight + 1
			if depth < int32(a.cfg.FinalityDepth) {
				if closed && !arming {
					arming = true
					a.armFinalityAsync(blockRegCh, log)
				}

				continue
			}

			log.InfoS(a.ctx, "Synthesizing confirmation done "+
				"from height-based safety depth",
				"confirm_height", a.confirmHeight,
				"current_height", epoch.Height,
				"finality_depth", int(a.cfg.FinalityDepth),
			)

			// Finality is terminal for the whole watch: deliver
			// Done (a no-op when only NotifyReorged was set) and
			// stop. A reorg-only watch therefore intentionally
			// stops receiving reorg events once the confirmation is
			// buried FinalityDepth deep — past that depth a reorg
			// is beyond the safety threshold the watch was created
			// to cover, so there is nothing left to observe.
			a.deliverConfDone(lastEvent)

			return

		case <-a.ctx.Done():
			log.InfoS(a.ctx, "ConfActor context cancelled")
			a.failConfirmation(a.ctx.Err())

			return
		}
	}
}

// applyPendingConfDone releases a terminal signal that was selected before the
// positive observation. False means there was no deferred terminal signal.
func (a *ConfActor) applyPendingConfDone(lastEvent *ConfirmationEvent,
	doneOrder *PositiveDoneOrder) bool {

	if !doneOrder.ObservePositive() {
		return false
	}

	a.deliverConfDone(lastEvent)

	return true
}

// applyConfReorg clears the current positive and any terminal signal that was
// selected early, then forwards the reversible observation to the subscriber.
func (a *ConfActor) applyConfReorg(lastEvent *ConfirmationEvent,
	doneOrder *PositiveDoneOrder) {

	a.deliverConfReorged(lastEvent)
	a.confirmHeight = 0
	doneOrder.ObserveReorg()
}

// applyConfDone forwards finality only after the matching positive observation
// supplied its identity and block metadata. False keeps the actor monitoring.
func (a *ConfActor) applyConfDone(lastEvent *ConfirmationEvent,
	doneOrder *PositiveDoneOrder) bool {

	if !doneOrder.ObserveDone() {
		// Disable the one-shot channel while the retained signal waits
		// for the positive observation.
		a.registration.Done = nil

		return false
	}

	a.deliverConfDone(lastEvent)

	return true
}

// clearFinalityRegistration cancels and forgets a dead block-epoch
// registration so the monitor loop can arm a replacement.
func (a *ConfActor) clearFinalityRegistration() {
	if a.blockReg == nil {
		return
	}

	a.blockReg.Cancel()
	a.blockReg = nil
}

// drainToLatestEpoch non-blockingly consumes any block epochs already
// queued on ch and returns the most recent non-nil epoch, starting from
// cur. With rapid-fire blocks (or a backend that re-delivers historical
// epochs) several epochs can sit in the channel at once; height-based
// finality synthesis depends only on the highest observed height, so
// collapsing the backlog to the newest epoch avoids re-evaluating the
// same monotonic Done condition once per stale epoch. The returned bool
// reports whether the channel was observed closed during the drain so the
// caller can park its receive on a nil channel.
func drainToLatestEpoch(ch <-chan *BlockEpoch,
	cur *BlockEpoch) (*BlockEpoch, bool) {

	latest := cur
	for {
		select {
		case e, ok := <-ch:
			if !ok {
				return latest, true
			}
			if e != nil {
				latest = e
			}

		default:
			return latest, false
		}
	}
}

// deliverConfirmation sends the confirmation event to the subscriber and
// completes the promise (Future mode) or sends to the actor (Actor mode).
// armFinalityAsync registers a block-epoch subscription for height-based
// finality synthesis off the actor's select loop. registerBlocksForFinality
// retries with a bounded backoff that can run for tens of seconds; doing it
// inline would block delivery of Reorged/Done/ctx.Done on this watch for the
// whole window. The registration (or nil on failure) is handed back on regCh,
// or cancelled if the actor exits before the loop reads it. The goroutine is
// tracked by the actor's wait group so Stop drains it.
func (a *ConfActor) armFinalityAsync(regCh chan<- finalityArmResult,
	log btclog.Logger) {

	a.wg.Go(func() {
		reg, err := registerBlocksForFinality(a.ctx, a.cfg.Backend, log)
		if err != nil {
			log.WarnS(a.ctx, "Giving up on height-based finality "+
				"synthesis; conf sub-actor will rely on "+
				"backend Done", err)
			reg = nil
		}

		// Capture the tip at arm time so the loop can finalize
		// immediately when the arming confirmation is already buried
		// past FinalityDepth (the block-epoch sub only delivers future
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

func (a *ConfActor) deliverConfirmation(event ConfirmationEvent) {
	a.promise.WhenSome(func(p actor.Promise[ConfirmationEvent]) {
		p.Complete(fn.Ok(event))
	})

	a.notifyActor.WhenSome(func(ref actor.TellOnlyRef[ConfirmationEvent]) {
		log := a.logger(a.ctx)
		if err := ref.Tell(a.ctx, event); err != nil {
			log.WarnS(a.ctx, "Failed to deliver confirmation", err)
		}
	})
}

// deliverConfReorged sends a reorg event to actor-mode subscribers. The
// correlation Txid is the registration's configured txid when set, since
// that is the identifier the caller asked us to watch; pkScript-only
// watches fall back to the txid carried on the most recent positive
// ConfirmationEvent.
func (a *ConfActor) deliverConfReorged(lastEvent *ConfirmationEvent) {
	var event ConfReorgedEvent
	switch {
	case a.txid != nil:
		event.Txid = *a.txid

	case lastEvent != nil:
		event.Txid = lastEvent.Txid
	}

	a.notifyReorged.WhenSome(func(ref actor.TellOnlyRef[ConfReorgedEvent]) {
		log := a.logger(a.ctx)
		if err := ref.Tell(a.ctx, event); err != nil {
			log.WarnS(a.ctx, "Failed to deliver confirmation reorg",
				err,
			)
		}
	})
}

// deliverConfDone sends a confirmation finality event to actor-mode
// subscribers. Txid follows the same precedence as deliverConfReorged.
func (a *ConfActor) deliverConfDone(lastEvent *ConfirmationEvent) {
	var event ConfDoneEvent
	switch {
	case a.txid != nil:
		event.Txid = *a.txid

	case lastEvent != nil:
		event.Txid = lastEvent.Txid
	}

	a.notifyDone.WhenSome(func(ref actor.TellOnlyRef[ConfDoneEvent]) {
		log := a.logger(a.ctx)
		if err := ref.Tell(a.ctx, event); err != nil {
			log.WarnS(a.ctx, "Failed to deliver confirmation done",
				err,
			)
		}
	})
}

// failConfirmation completes the promise with an error (Future mode) or does
// nothing (Actor mode - errors are not delivered in async mode).
func (a *ConfActor) failConfirmation(err error) {
	a.promise.WhenSome(func(p actor.Promise[ConfirmationEvent]) {
		p.Complete(fn.Err[ConfirmationEvent](err))
	})
}

// Stop gracefully shuts down the ConfActor. It cancels the context and waits
// for the monitoring goroutine to complete.
func (a *ConfActor) Stop() {
	// Cancel the context to signal shutdown.
	a.cancel()

	// Wait for the monitoring goroutine to complete.
	a.wg.Wait()
}

// OnStop implements actor.Stoppable for proper cleanup when stopped via actor
// system. This is called after the actor's message loop exits.
func (a *ConfActor) OnStop(ctx context.Context) error {
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

// buildConfirmationEvent converts the backend TxConfirmation into a
// ConfirmationEvent, filling in missing fields where possible.
func buildConfirmationEvent(details *TxConfirmation,
	watch *ConfActor) (ConfirmationEvent, error) {

	txHash, err := confTxHash(details, watch)
	if err != nil {
		return ConfirmationEvent{}, err
	}

	if details.BlockHash == nil {
		return ConfirmationEvent{}, fmt.Errorf("confirmation event " +
			"missing block hash")
	}

	event := ConfirmationEvent{
		Txid:        txHash,
		BlockHeight: int32(details.BlockHeight),
		BlockHash:   *details.BlockHash,
		NumConfs:    watch.targetConfs,
		Tx:          details.Tx,
		Block:       details.Block,
	}

	return event, nil
}

// confTxHash determines the transaction hash, falling back to deriving it
// from the txid if needed.
func confTxHash(details *TxConfirmation,
	watch *ConfActor) (chainhash.Hash, error) {

	switch {
	case details.Tx != nil:
		return details.Tx.TxHash(), nil

	case watch.txid != nil:
		return *watch.txid, nil

	default:
		return chainhash.Hash{}, fmt.Errorf("confirmation event " +
			"missing transaction hash")
	}
}
