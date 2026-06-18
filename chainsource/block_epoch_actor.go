package chainsource

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/build"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

const (
	// defaultBlockEpochReconnectBackoff is the initial delay before a
	// long-lived block subscription tries to re-register after its backend
	// stream closes. LND can close all chain notifier streams during
	// restart or internal notifier churn; callers such as the boarding
	// wallet need the subscription to heal without a daemon restart.
	defaultBlockEpochReconnectBackoff = time.Second

	// defaultBlockEpochMaxReconnectBackoff caps reconnect backoff so a
	// backend outage does not spin, but recovery still happens promptly.
	defaultBlockEpochMaxReconnectBackoff = 30 * time.Second

	// defaultBlockEpochFatalTimeout bounds how long a subscription may stay
	// continuously down before the actor stops retrying and escalates. A
	// notifier restart heals in seconds; a streak this long means the
	// backend connection is stuck (the production symptom was dozens of
	// subscribers re-registering against a dead notifier every 30s forever,
	// which no amount of in-process retrying can recover). Escalating lets
	// the daemon exit and restart with a fresh backend connection.
	defaultBlockEpochFatalTimeout = 5 * time.Minute

	// defaultBlockEpochFatalJitter is the maximum random padding added to
	// the fatal timeout on the default-timeout path. A backend outage tends
	// to drop every daemon's streams at the same instant; without jitter a
	// whole fleet would hit the 5m budget together and restart in lockstep
	// (thundering herd). Spreading the effective budget over a ~1m window
	// decorrelates those restarts.
	defaultBlockEpochFatalJitter = time.Minute
)

// errBlockEpochUnrecoverable is the placeholder cause used when a subscription
// stays down past the fatal timeout without a more specific reconnect error.
var errBlockEpochUnrecoverable = fmt.Errorf("block epoch backend unreachable")

// BlockEpochConfig holds configuration for BlockEpochActor.
type BlockEpochConfig struct {
	// Backend is the blockchain backend used to monitor blocks.
	Backend ChainBackend

	// Log is an optional logger for this actor instance. If None, the actor
	// falls back to extracting a logger from context via LoggerFromContext,
	// or uses btclog.Disabled if no logger is found.
	Log fn.Option[btclog.Logger]

	// ReconnectBackoff is the initial delay before reconnecting a block
	// epoch subscription whose backend stream closed after the initial
	// registration succeeded. Zero uses defaultBlockEpochReconnectBackoff.
	ReconnectBackoff time.Duration

	// MaxReconnectBackoff caps the exponential reconnect delay. Zero uses
	// defaultBlockEpochMaxReconnectBackoff.
	MaxReconnectBackoff time.Duration

	// FatalReconnectTimeout bounds how long the subscription may stay
	// continuously down before the actor stops retrying and escalates via
	// OnFatal. Zero uses defaultBlockEpochFatalTimeout.
	FatalReconnectTimeout time.Duration

	// OnFatal, when set, is invoked once if the subscription cannot be
	// re-established within FatalReconnectTimeout. The daemon wires this to
	// its shutdown path so a stuck backend connection restarts the process
	// instead of leaving every block subscriber silently spinning forever.
	OnFatal fn.Option[func(error)]

	// Now returns the current time. Tests override it to drive the fatal
	// timeout without real waits. Nil uses time.Now.
	Now func() time.Time

	// FatalReconnectJitter caps the random padding added to the fatal
	// timeout so a fleet sharing a backend outage does not escalate in
	// lockstep. On the default-timeout path (FatalReconnectTimeout zero) a
	// zero jitter uses defaultBlockEpochFatalJitter; an explicitly
	// configured timeout with zero jitter disables jitter so tests stay
	// deterministic.
	FatalReconnectJitter time.Duration

	// Rand returns a pseudo-random fraction in [0, 1) used to seed the
	// fatal timeout jitter. Nil uses a real random source. Tests override
	// it for determinism.
	Rand func() float64
}

// WithLogger returns a new config with the given logger set.
func (c BlockEpochConfig) WithLogger(log btclog.Logger) BlockEpochConfig {
	c.Log = fn.Some(log)

	return c
}

// BlockEpochActor is a single-subscription actor that monitors new blocks and
// delivers block epoch events. Each instance serves exactly one subscription.
//
// The actor supports dual-mode operation: Iterator mode for range-based
// iteration, and Actor mode for asynchronous event delivery to a registered
// actor. Each actor creates its own backend registration (no sharing).
type BlockEpochActor struct {
	// cfg holds all actor configuration including backend and optional
	// logger.
	cfg BlockEpochConfig

	// notifyActor is used in Actor mode to send events. None in Iterator
	// mode.
	notifyActor fn.Option[actor.TellOnlyRef[BlockEpoch]]

	// epochChan is used in Iterator mode to deliver blocks. Nil in Actor
	// mode.
	epochChan chan BlockEpoch

	// registration is the backend registration for this actor.
	registration *BlockRegistration

	// ctx is the actor's internal context for cancellation, created from
	// context.Background() to ensure it outlives any request context.
	//nolint:containedctx
	ctx context.Context

	// cancel cancels the actor's context.
	cancel context.CancelFunc

	// cancelFunc is the custom cancel function returned in the response.
	cancelFunc func()

	// wg tracks background goroutines for graceful shutdown.
	wg sync.WaitGroup
}

// NewBlockEpochActor creates a new BlockEpochActor instance with the given
// configuration. The config must include Backend; use WithLogger() to inject
// a specific logger.
func NewBlockEpochActor(cfg BlockEpochConfig) *BlockEpochActor {
	// Use background context for internal cancellation since the actor
	// needs to outlive any request context. Logger is passed via config.
	ctx, cancel := context.WithCancel(context.Background())

	return &BlockEpochActor{
		cfg:    cfg,
		ctx:    ctx,
		cancel: cancel,
	}
}

// logger returns the configured logger or falls back to extracting from
// context. If no logger is found in either location, returns btclog.Disabled.
func (a *BlockEpochActor) logger(ctx context.Context) btclog.Logger {
	return a.cfg.Log.UnwrapOr(build.LoggerFromContext(ctx))
}

// Receive processes incoming messages for the BlockEpochActor.
func (a *BlockEpochActor) Receive(actorCtx context.Context,
	msg EpochMsg) fn.Result[EpochResp] {

	switch m := msg.(type) {
	case *SubscribeBlocksRequest:
		return a.handleSubscribeBlocks(actorCtx, m)

	default:
		return fn.Err[EpochResp](
			fmt.Errorf("unknown message type: %T", msg),
		)
	}
}

// handleSubscribeBlocks processes a block subscription request. It configures
// the actor and starts monitoring.
func (a *BlockEpochActor) handleSubscribeBlocks(actorCtx context.Context,
	req *SubscribeBlocksRequest) fn.Result[EpochResp] {

	// Each BlockEpochActor instance serves exactly one subscription. Reject
	// duplicate registrations.
	if a.registration != nil {
		return fn.Err[EpochResp](
			fmt.Errorf("actor already has an active subscription"),
		)
	}

	// Configure the actor with request parameters.
	a.notifyActor = req.NotifyActor
	resp := &SubscribeBlocksResponse{}

	// Now we'll determine the notification mode: notify actor or iterator.
	if req.NotifyActor.IsSome() {
		// Actor mode: no channel needed.
		resp.Cancel = a.cancel
	} else {
		// In iterator mode we use a channel to funnel block epochs
		// from a listening goroutine into the main iterator. This lets
		// us consume block epochs as the iterator may block.
		a.epochChan = make(chan BlockEpoch, epochChannelSize)

		// Create an iter.Seq that reads from the channel. The sender
		// (monitorBlocks) is responsible for closing the channel, so we
		// only cancel the context here to signal shutdown.
		iterator := func(yield func(BlockEpoch) bool) {
			defer a.cancel()

			for {
				select {
				case epoch, ok := <-a.epochChan:
					if !ok {
						return
					}

					if !yield(epoch) {
						return
					}

				case <-a.ctx.Done():
					return
				}
			}
		}

		resp.Iterator = iterator
		resp.Cancel = a.cancel
		a.cancelFunc = a.cancel
	}

	// Register with the backend to receive block notifications. We do this
	// before starting the goroutine so we can return an error to the
	// caller if registration fails.
	//nolint:contextcheck // actor root context owns registration lifetime
	registration, err := a.cfg.Backend.RegisterBlocks(a.ctx)
	if err != nil {
		return fn.Err[EpochResp](
			fmt.Errorf("failed to register for blocks: %w", err),
		)
	}
	a.registration = registration

	// Now we'll make a goroutine to monitor blocks and forward events.
	a.wg.Add(1)
	go a.monitorBlocks()

	return fn.Ok[EpochResp](resp)
}

// blockEpochReconnectBackoff returns the normalized reconnect delay bounds for
// this actor. Tests can lower both values; production uses conservative
// defaults that avoid log spam while still healing notifier restarts promptly.
func (a *BlockEpochActor) blockEpochReconnectBackoff() (time.Duration,
	time.Duration) {

	initial := a.cfg.ReconnectBackoff
	if initial <= 0 {
		initial = defaultBlockEpochReconnectBackoff
	}

	maxBackoff := a.cfg.MaxReconnectBackoff
	if maxBackoff <= 0 {
		maxBackoff = defaultBlockEpochMaxReconnectBackoff
	}
	if maxBackoff < initial {
		maxBackoff = initial
	}

	return initial, maxBackoff
}

// now returns the current time via the configured clock, defaulting to the real
// clock. It lets tests drive the fatal timeout deterministically.
func (a *BlockEpochActor) now() time.Time {
	if a.cfg.Now != nil {
		return a.cfg.Now()
	}

	return time.Now()
}

// randFloat returns a pseudo-random fraction in [0, 1) via the configured
// source, defaulting to the global generator. It seeds the fatal timeout
// jitter; tests override it for determinism.
func (a *BlockEpochActor) randFloat() float64 {
	if a.cfg.Rand != nil {
		return a.cfg.Rand()
	}

	// The jitter only decorrelates restart timing across a fleet; it is not
	// security-sensitive, so a weak PRNG is fine here.
	//nolint:gosec // G404: jitter timing, not security-sensitive.
	return rand.Float64()
}

// fatalReconnectTimeout returns the normalized down-time budget before the
// actor escalates a stuck subscription, including a one-shot random jitter so a
// fleet that loses a shared backend does not escalate in lockstep. It is
// sampled once at the start of monitoring; reconnectExhausted takes the result
// as a parameter so the budget stays fixed across the reconnect loop.
func (a *BlockEpochActor) fatalReconnectTimeout() time.Duration {
	base := a.cfg.FatalReconnectTimeout
	jitter := a.cfg.FatalReconnectJitter
	if base <= 0 {
		base = defaultBlockEpochFatalTimeout

		// Only the default path opts into jitter automatically; an
		// explicit timeout stays exact unless jitter is set so tests
		// remain deterministic.
		if jitter <= 0 {
			jitter = defaultBlockEpochFatalJitter
		}
	}

	if jitter <= 0 {
		return base
	}

	return base + time.Duration(a.randFloat()*float64(jitter))
}

// reconnectExhausted reports whether the subscription has stayed continuously
// down past the given fatal timeout. A zero downSince means the subscription is
// currently healthy. The caller samples the timeout once so jitter does not
// wobble the budget between checks.
func (a *BlockEpochActor) reconnectExhausted(downSince time.Time,
	timeout time.Duration) bool {

	if downSince.IsZero() {
		return false
	}

	return a.now().Sub(downSince) >= timeout
}

// escalateFatal reports an unrecoverable subscription to the daemon via the
// configured OnFatal hook. cause is the last reconnect error, if any.
func (a *BlockEpochActor) escalateFatal(log btclog.Logger, downSince time.Time,
	cause error) {

	down := a.now().Sub(downSince).Round(time.Second)
	if cause == nil {
		cause = errBlockEpochUnrecoverable
	}
	err := fmt.Errorf("block epoch subscription unrecoverable after %s "+
		"down: %w", down, cause)

	log.ErrorS(a.ctx, "Block epoch subscription unrecoverable; escalating",
		err,
	)
	a.cfg.OnFatal.WhenSome(func(onFatal func(error)) {
		onFatal(err)
	})
}

// waitForReconnect sleeps for the current backoff unless the actor is
// stopping. It returns false when shutdown won the race.
func (a *BlockEpochActor) waitForReconnect(backoff time.Duration) bool {
	timer := time.NewTimer(backoff)
	defer timer.Stop()

	select {
	case <-timer.C:
		return true

	case <-a.ctx.Done():
		return false
	}
}

// nextReconnectBackoff doubles the reconnect delay and caps it at maxBackoff.
func nextReconnectBackoff(backoff, maxBackoff time.Duration) time.Duration {
	if backoff >= maxBackoff {
		return maxBackoff
	}

	next := backoff * 2
	if next > maxBackoff {
		return maxBackoff
	}

	return next
}

// monitorBlocks runs in a background goroutine and forwards block events to
// the subscriber (either channel or actor reference).
func (a *BlockEpochActor) monitorBlocks() {
	defer a.wg.Done()

	log := a.logger(a.ctx)
	log.InfoS(a.ctx, "BlockEpochActor monitoring started")

	reconnectBackoff, maxReconnectBackoff :=
		a.blockEpochReconnectBackoff()
	currentBackoff := reconnectBackoff
	registration := a.registration

	// downSince stamps when the subscription first went down; it is cleared
	// only once a replacement stream actually delivers a block. lastErr
	// holds the most recent reconnect failure so the eventual escalation
	// can surface a meaningful cause. Note the budget covers a stream that
	// is closed/reconnecting: an open stream that goes silent without
	// closing keeps the goroutine parked in the receive below and is out of
	// scope (a healthy backend delivers blocks well within the budget).
	var downSince time.Time
	var lastErr error

	// Sample the fatal budget once so its jitter stays fixed across every
	// reconnect attempt for this subscription.
	fatalTimeout := a.fatalReconnectTimeout()

	// In iterator mode, the sender (this goroutine) is responsible for
	// closing the channel to signal the receiver that no more values will
	// be sent. This follows Go's channel ownership semantics.
	defer func() {
		if a.epochChan != nil {
			close(a.epochChan)
		}
	}()

	// Make sure we clean up the registration on exit.
	defer func() {
		log.InfoS(a.ctx, "BlockEpochActor monitoring stopped")
		if registration != nil {
			registration.Cancel()
		}
	}()

	for {
		if registration == nil {
			// A normal shutdown cancels the actor context. Bail out
			// before evaluating the fatal budget so a stop that
			// coincides with a long outage exits cleanly instead of
			// escalating a fatal failure the daemon does not really
			// have (escalation latches the health flag).
			if a.ctx.Err() != nil {
				return
			}

			// If the subscription has stayed down past the fatal
			// budget, stop retrying and escalate so the daemon can
			// restart with a fresh backend connection. Only callers
			// that opted into bounded retries by installing OnFatal
			// take this path; without a hook there is nowhere to
			// escalate, so we preserve the unbounded-retry contract
			// and keep trying to heal in-process rather than
			// silently killing the subscription.
			if a.cfg.OnFatal.IsSome() &&
				a.reconnectExhausted(downSince, fatalTimeout) {

				a.escalateFatal(log, downSince, lastErr)

				return
			}

			if !a.waitForReconnect(currentBackoff) {
				return
			}

			var err error
			registration, err = a.cfg.Backend.RegisterBlocks(a.ctx)
			if err != nil {
				lastErr = err
				log.WarnS(a.ctx, "Block epoch reconnect failed",
					err,
					slog.Duration("backoff",
						currentBackoff),
				)
				currentBackoff = nextReconnectBackoff(
					currentBackoff, maxReconnectBackoff,
				)

				continue
			}

			log.InfoS(a.ctx, "Block epoch subscription reconnected")
			currentBackoff = reconnectBackoff
		}

		select {
		case epoch, ok := <-registration.Epochs:
			if !ok {
				// Stamp the start of the down streak on the
				// first loss; a successful re-registration that
				// immediately closes again must not reset it,
				// or a storming backend would never escalate.
				if downSince.IsZero() {
					downSince = a.now()
				}

				log.InfoS(
					a.ctx,
					"Block epoch channel closed, "+
						"reconnecting",
					slog.Duration("backoff",
						currentBackoff),
				)
				registration.Cancel()
				registration = nil

				continue
			}

			// A delivered block proves the subscription is healthy
			// again, so clear the down streak and last error.
			downSince = time.Time{}
			lastErr = nil

			log.InfoS(a.ctx, "Received block from backend",
				slog.Int("height", int(epoch.Height)),
			)

			// Forward the block epoch from the backend.
			blockEpoch := BlockEpoch{
				Height:    epoch.Height,
				Hash:      epoch.Hash,
				Timestamp: epoch.Timestamp,
			}

			// If there's an epoch channel, then we're in iterator
			// mode.
			if a.epochChan != nil {
				select {
				case a.epochChan <- blockEpoch:
				case <-a.ctx.Done():
					return
				}
			} else {
				// Otherwise, this is actor mode, so we'll
				// deliver in the block epoch via a Tell.
				log.InfoS(
					a.ctx,
					"Forwarding block to notify actor",
					slog.Int(
						"height",
						int(blockEpoch.Height),
					),
				)

				notifyRef := func(
					ref actor.TellOnlyRef[BlockEpoch]) {

					err := ref.Tell(a.ctx, blockEpoch)
					if err != nil {
						log.WarnS(
							a.ctx,
							"Failed to deliver "+
								"block epoch",
							err,
						)
					}
				}
				a.notifyActor.WhenSome(notifyRef)
			}

		case <-a.ctx.Done():
			log.InfoS(a.ctx, "BlockEpochActor context cancelled")

			return
		}
	}
}

// Stop gracefully shuts down the BlockEpochActor. It cancels the context and
// waits for the monitoring goroutine to complete.
func (a *BlockEpochActor) Stop() {
	a.cancel()

	a.wg.Wait()
}

// OnStop implements actor.Stoppable for proper cleanup when stopped via actor
// system. This is called after the actor's message loop exits.
func (a *BlockEpochActor) OnStop(ctx context.Context) error {
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
