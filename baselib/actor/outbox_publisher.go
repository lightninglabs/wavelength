package actor

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"
)

// OutboxPublisherConfig holds configuration for the OutboxPublisher.
type OutboxPublisherConfig struct {
	// Store is the persistence layer for outbox operations.
	Store DeliveryStore

	// Codec handles message deserialization.
	Codec *MessageCodec

	// System provides access to the receptionist for actor discovery.
	System SystemContext

	// PollInterval is how often to poll for pending outbox messages.
	// Same-process outbox commits wake the publisher immediately, so polling
	// is only the fallback for missed wakes and process restarts.
	// Default: 1s.
	PollInterval time.Duration

	// BatchSize is the maximum number of messages to process per poll.
	// Default: 100.
	BatchSize int

	// MaxDeliveryAttempts is the maximum delivery attempts before dead-lettering.
	// Default: 10.
	MaxDeliveryAttempts int

	// ClaimDuration is how long the publisher holds a claim on outbox
	// messages. After expiry, uncompleted messages become available for
	// reclaim by another publisher instance. Default: 30s.
	ClaimDuration time.Duration
}

// DefaultOutboxPublisherConfig returns configuration with sensible defaults.
func DefaultOutboxPublisherConfig(
	store DeliveryStore,
	codec *MessageCodec,
	system SystemContext,
) OutboxPublisherConfig {

	return OutboxPublisherConfig{
		Store:               store,
		Codec:               codec,
		System:              system,
		PollInterval:        time.Second,
		BatchSize:           100,
		MaxDeliveryAttempts: 10,
		ClaimDuration:       30 * time.Second,
	}
}

// OutboxPublisher is a background service that drains the transactional outbox
// and delivers messages to target actors. It implements the CDC (Change Data
// Capture) pattern: messages written to the outbox during a transaction are
// delivered after the transaction commits.
//
// The publisher:
//   - Polls the outbox for pending messages
//   - Looks up target actors via ServiceKey (using target_actor_id as key name)
//   - Delivers messages using Tell (fire-and-forget)
//   - Marks messages complete after successful delivery
//   - Moves messages to dead letter queue after max attempts
type OutboxPublisher struct {
	cfg OutboxPublisherConfig

	// ctx is the publisher's lifecycle context.
	ctx context.Context

	// cancel cancels the publisher's context.
	cancel context.CancelFunc

	// wg tracks the background goroutine.
	wg sync.WaitGroup

	// wake nudges the publisher when same-process outbox work commits.
	wake chan struct{}

	// startOnce ensures Run is only called once.
	startOnce sync.Once

	// stopOnce ensures Stop is only called once.
	stopOnce sync.Once
}

// NewOutboxPublisher creates a new outbox publisher.
func NewOutboxPublisher(cfg OutboxPublisherConfig) *OutboxPublisher {
	ctx, cancel := context.WithCancel(context.Background())

	if cfg.PollInterval == 0 {
		cfg.PollInterval = time.Second
	}
	if cfg.BatchSize == 0 {
		cfg.BatchSize = 100
	}
	if cfg.MaxDeliveryAttempts == 0 {
		cfg.MaxDeliveryAttempts = 10
	}
	if cfg.ClaimDuration == 0 {
		cfg.ClaimDuration = 30 * time.Second
	}

	p := &OutboxPublisher{
		cfg:    cfg,
		ctx:    ctx,
		cancel: cancel,
		wake:   make(chan struct{}, 1),
	}

	if registrar, ok := cfg.Store.(OutboxWakeRegistrar); ok {
		registrar.RegisterOutboxWake(p.Wake)
	}

	return p
}

// Start begins the background publishing loop.
func (p *OutboxPublisher) Start() {
	p.startOnce.Do(func() {
		logger(p.ctx).DebugS(p.ctx, "Starting outbox publisher",
			"poll_interval", p.cfg.PollInterval,
			"batch_size", p.cfg.BatchSize)

		p.wg.Add(1)
		go p.run()
	})
}

// Stop signals the publisher to terminate and waits for it to finish.
func (p *OutboxPublisher) Stop() {
	p.stopOnce.Do(func() {
		p.cancel()
		p.wg.Wait()

		logger(p.ctx).DebugS(p.ctx, "Outbox publisher stopped")
	})
}

// run is the main publishing loop.
func (p *OutboxPublisher) run() {
	defer p.wg.Done()

	ticker := time.NewTicker(p.cfg.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-p.ctx.Done():
			return

		case <-ticker.C:
			p.publishBatch()

		case <-p.wake:
			p.publishBatch()
		}
	}
}

// publishBatch claims and delivers a batch of pending outbox messages.
func (p *OutboxPublisher) publishBatch() {
	// Generate a unique claim token for this batch. All messages in the
	// batch share the same token so CompleteOutbox/FailOutbox can
	// validate ownership.
	claimToken := uuid.Must(uuid.NewV7()).String()

	messages, err := p.cfg.Store.ClaimOutboxBatch(
		p.ctx, OutboxClaimParams{
			Limit:         p.cfg.BatchSize,
			ClaimToken:    claimToken,
			ClaimDuration: p.cfg.ClaimDuration,
		},
	)
	if err != nil {
		logger(p.ctx).WarnS(p.ctx, "Failed to claim outbox batch", err)
		return
	}

	if len(messages) == 0 {
		return
	}

	logger(p.ctx).TraceS(p.ctx, "Processing outbox batch",
		"count", len(messages),
		"claim_token", claimToken)

	for _, msg := range messages {
		p.deliverMessage(msg)
	}
}

// deliverMessage attempts to deliver a single outbox message.
func (p *OutboxPublisher) deliverMessage(msg OutboxMessage) {
	// Check if max delivery attempts exceeded. ClaimOutboxBatch already
	// incremented DeliveryAttempts, so we check against the configured max.
	if msg.DeliveryAttempts > p.cfg.MaxDeliveryAttempts {
		logger(p.ctx).WarnS(p.ctx, "Outbox message exceeded max delivery attempts",
			nil,
			"message_id", msg.ID,
			"target", msg.TargetActorID,
			"attempts", msg.DeliveryAttempts,
			"max_attempts", p.cfg.MaxDeliveryAttempts)

		dlErr := p.cfg.Store.FailOutbox(
			p.ctx, msg.ID, msg.ClaimToken,
		)
		if dlErr != nil {
			logger(p.ctx).WarnS(p.ctx,
				"Failed to dead-letter outbox message",
				dlErr, "message_id", msg.ID)
		}

		return
	}

	// Decode the message payload.
	decoded, err := p.cfg.Codec.Decode(msg.Payload)
	if err != nil {
		logger(p.ctx).WarnS(p.ctx, "Failed to decode outbox message", err,
			"message_id", msg.ID,
			"message_type", msg.MessageType)

		// Poison pill - mark as failed (dead letter).
		dlErr := p.cfg.Store.FailOutbox(
			p.ctx, msg.ID, msg.ClaimToken,
		)
		if dlErr != nil {
			logger(p.ctx).WarnS(p.ctx,
				"Failed to dead-letter outbox message",
				dlErr, "message_id", msg.ID)
		}

		return
	}

	// Create a service key for the target. The target_actor_id is treated
	// as a service key name. Since we don't know the exact types at runtime,
	// we use Message/any as the generic parameters.
	targetKey := NewServiceKey[Message, any](msg.TargetActorID)

	// Get a router for the target service key.
	ref := targetKey.Ref(p.cfg.System)

	// Inject the outbox message ID into the context so the target
	// actor's DurableMailbox uses it as the inbox message ID. This
	// enables receiver-side deduplication: if CompleteOutbox fails
	// after a successful Tell, the retry inserts the same ID and the
	// ON CONFLICT clause makes it a no-op.
	deliverCtx := WithOutboxID(p.ctx, msg.ID)

	// Deliver the message. Tell now returns an error if the message could
	// not be durably enqueued to the target's mailbox.
	if err := ref.Tell(deliverCtx, decoded); err != nil {
		logger(p.ctx).WarnS(p.ctx, "Failed to deliver outbox message", err,
			"message_id", msg.ID,
			"target", msg.TargetActorID,
			"attempts", msg.DeliveryAttempts)

		// Don't mark as complete - leave for retry on next poll.
		// The message will be dead-lettered when DeliveryAttempts exceeds
		// MaxDeliveryAttempts (checked at the start of this function).
		return
	}

	// Mark as complete after successful durable send.
	completeErr := p.cfg.Store.CompleteOutbox(
		p.ctx, msg.ID, msg.ClaimToken,
	)
	if completeErr != nil {
		logger(p.ctx).WarnS(p.ctx, "Failed to complete outbox message",
			completeErr, "message_id", msg.ID)
	}

	logger(p.ctx).TraceS(p.ctx, "Delivered outbox message",
		"message_id", msg.ID,
		"source", msg.SourceActorID,
		"target", msg.TargetActorID,
		"message_type", msg.MessageType)
}

// PublishPending manually triggers a publish cycle. This is useful for testing
// or when immediate delivery is needed after a transaction commits.
func (p *OutboxPublisher) PublishPending() {
	p.publishBatch()
}

// Wake asks the publisher to run a publish cycle soon. The signal is
// best-effort because the periodic poll remains the durability fallback.
func (p *OutboxPublisher) Wake() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}
