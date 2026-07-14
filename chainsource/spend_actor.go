package chainsource

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/build"
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

	// registration is the backend registration for this watch.
	registration *SpendRegistration

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

	// Configure the actor with request parameters.
	a.outpoint = req.Outpoint
	a.pkScript = req.PkScript
	a.heightHint = req.HeightHint
	a.notifyActor = req.NotifyActor

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
	// caller if registration fails.
	//nolint:contextcheck // actor root context owns registration lifetime
	registration, err := a.cfg.Backend.RegisterSpend(
		a.ctx, a.outpoint, a.pkScript, a.heightHint,
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
	}()

	// Monitor for spends indefinitely until cancelled or shutdown.
	// This allows us to catch re-org events where a spend is replaced.
	for {
		select {
		case spend, ok := <-a.registration.Spend:
			if !ok || spend == nil {
				a.failSpend(
					errors.New("spend subscription closed"),
				)

				return
			}

			event, err := buildSpendEvent(spend, a)
			if err != nil {
				a.failSpend(err)

				return
			}

			// Deliver the event.
			a.deliverSpend(event)

			// In Future mode, exit after first event. In Actor
			// mode, continue monitoring for re-org events.
			if a.promise.IsSome() {
				return
			}

		case <-a.ctx.Done():
			// Actor was cancelled.
			a.failSpend(a.ctx.Err())

			return
		}
	}
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
