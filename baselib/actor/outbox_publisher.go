package actor

import (
	"context"
	"sync"
	"time"
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
	// Default: 100ms.
	PollInterval time.Duration

	// BatchSize is the maximum number of messages to process per poll.
	// Default: 100.
	BatchSize int

	// MaxDeliveryAttempts is the maximum delivery attempts before dead-lettering.
	// Default: 10.
	MaxDeliveryAttempts int
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
		PollInterval:        100 * time.Millisecond,
		BatchSize:           100,
		MaxDeliveryAttempts: 10,
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

	// startOnce ensures Run is only called once.
	startOnce sync.Once

	// stopOnce ensures Stop is only called once.
	stopOnce sync.Once
}

// NewOutboxPublisher creates a new outbox publisher.
func NewOutboxPublisher(cfg OutboxPublisherConfig) *OutboxPublisher {
	ctx, cancel := context.WithCancel(context.Background())

	if cfg.PollInterval == 0 {
		cfg.PollInterval = 100 * time.Millisecond
	}
	if cfg.BatchSize == 0 {
		cfg.BatchSize = 100
	}
	if cfg.MaxDeliveryAttempts == 0 {
		cfg.MaxDeliveryAttempts = 10
	}

	return &OutboxPublisher{
		cfg:    cfg,
		ctx:    ctx,
		cancel: cancel,
	}
}

// Start begins the background publishing loop.
func (p *OutboxPublisher) Start() {
	p.startOnce.Do(func() {
		log.DebugS(p.ctx, "Starting outbox publisher",
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

		log.DebugS(context.Background(), "Outbox publisher stopped")
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
		}
	}
}

// publishBatch claims and delivers a batch of pending outbox messages.
func (p *OutboxPublisher) publishBatch() {
	messages, err := p.cfg.Store.ClaimOutboxBatch(p.ctx, p.cfg.BatchSize)
	if err != nil {
		log.WarnS(p.ctx, "Failed to claim outbox batch", err)
		return
	}

	if len(messages) == 0 {
		return
	}

	log.TraceS(p.ctx, "Processing outbox batch",
		"count", len(messages))

	for _, msg := range messages {
		p.deliverMessage(msg)
	}
}

// deliverMessage attempts to deliver a single outbox message.
func (p *OutboxPublisher) deliverMessage(msg OutboxMessage) {
	// Decode the message payload.
	decoded, err := p.cfg.Codec.Decode(msg.Payload)
	if err != nil {
		log.WarnS(p.ctx, "Failed to decode outbox message", err,
			"message_id", msg.ID,
			"message_type", msg.MessageType)

		// Poison pill - mark as failed (dead letter).
		if dlErr := p.cfg.Store.FailOutbox(p.ctx, msg.ID); dlErr != nil {
			log.WarnS(p.ctx, "Failed to dead-letter outbox message",
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

	// Deliver the message. Tell now returns an error if the message could
	// not be durably enqueued to the target's mailbox.
	if err := ref.Tell(p.ctx, decoded); err != nil {
		log.WarnS(p.ctx, "Failed to deliver outbox message", err,
			"message_id", msg.ID,
			"target", msg.TargetActorID)

		// Don't mark as complete - leave for retry on next poll.
		// If this consistently fails, it will eventually be dead-lettered
		// by max delivery attempts (tracked in outbox_messages table).
		return
	}

	// Mark as complete after successful durable send.
	if err := p.cfg.Store.CompleteOutbox(p.ctx, msg.ID); err != nil {
		log.WarnS(p.ctx, "Failed to complete outbox message", err,
			"message_id", msg.ID)
	}

	log.TraceS(p.ctx, "Delivered outbox message",
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
