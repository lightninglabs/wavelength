//nolint:ll
package oor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/google/uuid"
	"github.com/lightningnetwork/lnd/clock"
)

const (
	OORClientEffectRequestArkSignatures        = "request_ark_signatures"
	OORClientEffectSendSubmitPackage           = "send_submit_package"
	OORClientEffectRequestCheckpointSignatures = "request_checkpoint_signatures"
	OORClientEffectSendFinalizePackage         = "send_finalize_package"
	OORClientEffectMarkInputsSpent             = "mark_inputs_spent"
	OORClientEffectQueryIncomingTransfer       = "query_incoming_transfer"
	OORClientEffectQueryIncomingMetadata       = "query_incoming_metadata"
	OORClientEffectMaterializeIncomingVTXOs    = "materialize_incoming_vtxos"
	OORClientEffectSendIncomingAck             = "send_incoming_ack"

	defaultOORClientEffectBatchSize = 8
	defaultOORClientEffectLease     = 30 * time.Second
	defaultOORClientEffectInterval  = time.Second
	defaultOORClientEffectRetry     = 2 * time.Second
)

// ErrOORClientEffectAwaitingExternalAck means the effect was handed to an
// external durable boundary and must remain claim-held until the session
// advances or the claim expires for crash replay.
var ErrOORClientEffectAwaitingExternalAck = errors.New("oor client effect " +
	"awaiting external acknowledgement")

// OORClientEffectID returns the stable durable id for a client OOR effect.
func OORClientEffectID(sessionID SessionID, effectType string) string {
	return fmt.Sprintf("oor-client/%s/%s", sessionID.String(), effectType)
}

// EffectTypeForOutbox returns the SQL effect discriminator for an outbox event.
func EffectTypeForOutbox(outbox OutboxEvent) (string, bool) {
	switch outbox.(type) {
	case *RequestArkSignatures:
		return OORClientEffectRequestArkSignatures, true

	case *SendSubmitPackageRequest:
		return OORClientEffectSendSubmitPackage, true

	case *RequestCheckpointSignatures:
		return OORClientEffectRequestCheckpointSignatures, true

	case *SendFinalizePackageRequest:
		return OORClientEffectSendFinalizePackage, true

	case *MarkInputsSpentRequest:
		return OORClientEffectMarkInputsSpent, true

	case *QueryIncomingTransferRequest:
		return OORClientEffectQueryIncomingTransfer, true

	case *QueryIncomingMetadataRequest:
		return OORClientEffectQueryIncomingMetadata, true

	case *MaterializeIncomingVTXOsRequest:
		return OORClientEffectMaterializeIncomingVTXOs, true

	case *SendIncomingAckRequest:
		return OORClientEffectSendIncomingAck, true

	default:
		return "", false
	}
}

// OORClientEffect is a claimed client OOR effect row.
type OORClientEffect struct {
	ID         string
	SessionID  SessionID
	Direction  SessionDirection
	EffectType string
	ClaimToken string
	Attempts   int32
}

// OORClientEffectStore is the SQL lease/retry boundary for client OOR effects.
type OORClientEffectStore interface {
	ClaimDueOORClientEffects(ctx context.Context, owner string, limit int,
		lease time.Duration) ([]OORClientEffect, error)

	MarkOORClientEffectDone(ctx context.Context, id,
		claimToken string) error

	ReleaseOORClientEffectForRetry(ctx context.Context, id,
		claimToken string, retryAfter time.Duration,
		failure error) error

	ReleaseExpiredOORClientEffectClaims(ctx context.Context) error
}

// OORClientEffectProcessor executes one claimed effect against the in-memory
// coordinator/actor facade.
type OORClientEffectProcessor interface {
	ProcessOORClientEffect(ctx context.Context,
		effect OORClientEffect) error
}

// OORClientEffectWorker drains durable client OOR effect rows.
type OORClientEffectWorker struct {
	store     OORClientEffectStore
	processor OORClientEffectProcessor
	clk       clock.Clock
	log       btclog.Logger

	owner      string
	batchSize  int
	lease      time.Duration
	interval   time.Duration
	retryDelay time.Duration

	cancel context.CancelFunc
	done   chan struct{}
}

// OORClientEffectWorkerConfig configures a client OOR effect worker.
type OORClientEffectWorkerConfig struct {
	Store     OORClientEffectStore
	Processor OORClientEffectProcessor
	Clock     clock.Clock
	Logger    btclog.Logger

	Owner      string
	BatchSize  int
	Lease      time.Duration
	Interval   time.Duration
	RetryDelay time.Duration
}

// NewOORClientEffectWorker creates a client OOR effect worker.
func NewOORClientEffectWorker(
	cfg OORClientEffectWorkerConfig) *OORClientEffectWorker {

	if cfg.Clock == nil {
		cfg.Clock = clock.NewDefaultClock()
	}
	if cfg.Logger == nil {
		cfg.Logger = btclog.Disabled
	}
	if cfg.Owner == "" {
		cfg.Owner = "oor-client-effects-" + uuid.NewString()
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = defaultOORClientEffectBatchSize
	}
	if cfg.Lease <= 0 {
		cfg.Lease = defaultOORClientEffectLease
	}
	if cfg.Interval <= 0 {
		cfg.Interval = defaultOORClientEffectInterval
	}
	if cfg.RetryDelay <= 0 {
		cfg.RetryDelay = defaultOORClientEffectRetry
	}

	return &OORClientEffectWorker{
		store:      cfg.Store,
		processor:  cfg.Processor,
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

// Start begins the polling loop.
func (w *OORClientEffectWorker) Start(ctx context.Context) error {
	if w.store == nil {
		return fmt.Errorf("client OOR effect store must be provided")
	}
	if w.processor == nil {
		return fmt.Errorf("client OOR effect processor must be " +
			"provided")
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
func (w *OORClientEffectWorker) Stop() {
	if w.cancel != nil {
		w.cancel()
	}
	if w.done != nil {
		<-w.done
	}
}

func (w *OORClientEffectWorker) loop(ctx context.Context) {
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
			w.log.WarnS(ctx, "Client OOR effect worker tick failed",
				err,
			)
		}
	}
}

// RunOnce claims and processes one batch of due effects.
func (w *OORClientEffectWorker) RunOnce(ctx context.Context) error {
	if err := w.store.ReleaseExpiredOORClientEffectClaims(ctx); err != nil {
		return err
	}

	effects, err := w.store.ClaimDueOORClientEffects(
		ctx, w.owner, w.batchSize, w.lease,
	)
	if err != nil {
		return err
	}

	for _, effect := range effects {
		if err := w.processor.ProcessOORClientEffect(
			ctx, effect,
		); err != nil {

			if errors.Is(err, ErrOORClientEffectAwaitingExternalAck) {
				continue
			}

			w.log.WarnS(ctx, "Client OOR effect failed",
				err,
				slog.String("effect_id", effect.ID),
				slog.String("effect_type", effect.EffectType),
				slog.Int("attempts", int(effect.Attempts)),
			)

			if releaseErr := w.store.ReleaseOORClientEffectForRetry(
				ctx, effect.ID, effect.ClaimToken, w.retryDelay,
				err,
			); releaseErr != nil {
				return releaseErr
			}

			continue
		}

		if err := w.store.MarkOORClientEffectDone(
			ctx, effect.ID, effect.ClaimToken,
		); err != nil {
			return err
		}
	}

	return nil
}
