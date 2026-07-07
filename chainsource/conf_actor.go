package chainsource

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/build"
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

	// registration is the backend registration for this watch.
	registration *ConfRegistration

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

	a.txid = req.Txid
	a.pkScript = req.PkScript
	a.targetConfs = req.TargetConfs
	a.heightHint = req.HeightHint
	a.includeBlock = req.IncludeBlock
	a.notifyActor = req.NotifyActor

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
// confirmation count to be reached. When reached, it delivers the event and
// exits.
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
	}()

	select {
	case confDetails, ok := <-a.registration.Confirmed:
		if !ok || confDetails == nil {
			log.WarnS(a.ctx, "Confirmation subscription closed",
				fmt.Errorf("channel closed or nil details"),
			)
			a.failConfirmation(
				fmt.Errorf("confirmation subscription closed"),
			)

			return
		}

		log.InfoS(a.ctx, "Received confirmation from backend",
			"block_height", confDetails.BlockHeight,
			"block_hash", confDetails.BlockHash,
		)

		event, err := buildConfirmationEvent(confDetails, a)
		if err != nil {
			log.WarnS(a.ctx, "Failed to build confirmation event",
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

	case <-a.ctx.Done():
		log.InfoS(a.ctx, "ConfActor context cancelled")
		a.failConfirmation(a.ctx.Err())
	}
}

// deliverConfirmation sends the confirmation event to the subscriber and
// completes the promise (Future mode) or sends to the actor (Actor mode).
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
