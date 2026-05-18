package unroll

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/google/uuid"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/db"
	"github.com/lightningnetwork/lnd/clock"
)

const (
	defaultEffectBatchSize = 16
	defaultEffectLease     = 30 * time.Second
	defaultEffectInterval  = time.Second
	defaultEffectRetry     = 2 * time.Second

	unrollEffectSubscribeBlocks         = "subscribe_blocks"
	unrollEffectWatchTargetSpend        = "watch_target_spend"
	unrollEffectEnsureTxConfirmed       = "ensure_tx_confirmed"
	unrollEffectWatchDeferredCheckpoint = "watch_deferred_checkpoint"
	unrollEffectBuildSweep              = "build_sweep"
	unrollEffectEnsureSweepConfirmed    = "ensure_sweep_confirmed"
	unrollEffectNotifyRegistry          = "notify_registry"
)

var validUnrollEffectTypes = map[string]struct{}{
	unrollEffectSubscribeBlocks:         {},
	unrollEffectWatchTargetSpend:        {},
	unrollEffectEnsureTxConfirmed:       {},
	unrollEffectWatchDeferredCheckpoint: {},
	unrollEffectBuildSweep:              {},
	unrollEffectEnsureSweepConfirmed:    {},
	unrollEffectNotifyRegistry:          {},
}

// EffectStore is the SQL retry surface for unroll side effects.
type EffectStore interface {
	ClaimDueEffects(ctx context.Context, owner string, limit int,
		lease time.Duration) ([]db.UnrollEffectRecord, error)

	MarkEffectDone(ctx context.Context, id, claimToken string) error

	ReleaseEffectForRetry(ctx context.Context, id, claimToken string,
		retryAfter time.Duration, failure error) error

	ReleaseExpiredEffectClaims(ctx context.Context) error
}

// EffectWorker drains pending unroll effect rows. The per-target actor still
// owns the domain logic; the worker is the crash-recovery pump that turns a
// stranded SQL effect row into a target resume.
type EffectWorker struct {
	store    EffectStore
	registry actor.ActorRef[RegistryMsg, RegistryResp]
	clk      clock.Clock
	log      btclog.Logger

	owner      string
	batchSize  int
	lease      time.Duration
	interval   time.Duration
	retryDelay time.Duration

	cancel context.CancelFunc
	done   chan struct{}
}

// EffectWorkerConfig configures the unroll effect worker.
type EffectWorkerConfig struct {
	Store    EffectStore
	Registry actor.ActorRef[RegistryMsg, RegistryResp]
	Clock    clock.Clock
	Logger   btclog.Logger

	Owner      string
	BatchSize  int
	Lease      time.Duration
	Interval   time.Duration
	RetryDelay time.Duration
}

// NewEffectWorker creates an unroll effect worker.
func NewEffectWorker(cfg EffectWorkerConfig) *EffectWorker {
	if cfg.Clock == nil {
		cfg.Clock = clock.NewDefaultClock()
	}
	if cfg.Logger == nil {
		cfg.Logger = btclog.Disabled
	}
	if cfg.Owner == "" {
		cfg.Owner = "unroll-effects-" + uuid.NewString()
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = defaultEffectBatchSize
	}
	if cfg.Lease <= 0 {
		cfg.Lease = defaultEffectLease
	}
	if cfg.Interval <= 0 {
		cfg.Interval = defaultEffectInterval
	}
	if cfg.RetryDelay <= 0 {
		cfg.RetryDelay = defaultEffectRetry
	}

	return &EffectWorker{
		store:      cfg.Store,
		registry:   cfg.Registry,
		clk:        cfg.Clock,
		log:        cfg.Logger,
		owner:      cfg.Owner,
		batchSize:  cfg.BatchSize,
		lease:      cfg.Lease,
		interval:   cfg.Interval,
		retryDelay: cfg.RetryDelay,
		done:       make(chan struct{}),
	}
}

// Start begins the effect polling loop.
func (w *EffectWorker) Start(ctx context.Context) error {
	if w.store == nil {
		return fmt.Errorf("unroll effect store must be provided")
	}
	if w.registry == nil {
		return fmt.Errorf("unroll registry ref must be provided")
	}
	if w.cancel != nil {
		return nil
	}

	runCtx, cancel := context.WithCancel(ctx)
	w.cancel = cancel
	go w.loop(runCtx)

	return nil
}

// Stop stops the worker and waits for exit.
func (w *EffectWorker) Stop() {
	if w == nil || w.cancel == nil {
		return
	}
	w.cancel()
	if w.done != nil {
		<-w.done
	}
}

func (w *EffectWorker) loop(ctx context.Context) {
	defer close(w.done)

	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case <-ticker.C:
		}

		if err := w.RunOnce(ctx); err != nil && ctx.Err() == nil {
			w.log.WarnS(ctx, "Unroll effect worker tick failed",
				err,
			)
		}
	}
}

// RunOnce claims and processes one batch of due effects.
func (w *EffectWorker) RunOnce(ctx context.Context) error {
	if err := w.store.ReleaseExpiredEffectClaims(ctx); err != nil {
		return err
	}

	effects, err := w.store.ClaimDueEffects(
		ctx, w.owner, w.batchSize, w.lease,
	)
	if err != nil {
		return err
	}

	for _, effect := range effects {
		if err := w.handleEffect(ctx, effect); err != nil {
			w.log.WarnS(ctx, "Unroll effect failed", err,
				slog.String("effect_id", effect.ID),
				slog.String("effect_type", effect.EffectType),
				slog.String(
					"outpoint",
					effect.TargetOutpoint.String(),
				),
				slog.Int("attempts", int(effect.Attempts)))

			token := effect.ClaimToken.String
			releaseErr := w.store.ReleaseEffectForRetry(
				ctx, effect.ID, token, w.retryDelay, err,
			)
			if releaseErr != nil {
				return releaseErr
			}

			continue
		}

		if err := w.store.MarkEffectDone(
			ctx, effect.ID, effect.ClaimToken.String,
		); err != nil {
			return err
		}
	}

	return nil
}

func (w *EffectWorker) handleEffect(ctx context.Context,
	effect db.UnrollEffectRecord) error {

	if _, ok := validUnrollEffectTypes[effect.EffectType]; !ok {
		return fmt.Errorf("unknown unroll effect type %q",
			effect.EffectType)
	}

	_, err := w.registry.Ask(ctx, &replayUnrollEffectMsg{
		Outpoint: effect.TargetOutpoint,
	}).Await(ctx).Unpack()

	return err
}

type replayUnrollEffectMsg struct {
	actor.BaseMessage

	Outpoint wire.OutPoint
}

func (m *replayUnrollEffectMsg) MessageType() string {
	return "replayUnrollEffectMsg"
}

func (m *replayUnrollEffectMsg) registryMsgSealed() {}
