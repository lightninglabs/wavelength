-- Durable mailbox migration.
-- This migration creates tables for persistent actor mailboxes with
-- lease-based delivery, transactional outbox for CDC, and deduplication.
--
-- Reference: The design is based on the durable mailbox gist pattern with
-- lease-based ownership to prevent stale-ack races and ensure exactly-once
-- effects on top of at-least-once delivery.

-- Actor mailbox messages table.
-- Stores incoming messages for each actor with lease-based delivery semantics.
-- Messages are leased to a consumer who must Ack/Nack before lease expires,
-- otherwise the message becomes available for redelivery.
CREATE TABLE IF NOT EXISTS mailbox_messages (
    -- id is a ULID providing time-ordering and uniqueness.
    id TEXT PRIMARY KEY,

    -- mailbox_id identifies the target actor's mailbox.
    mailbox_id TEXT NOT NULL,

    -- message_type is the type name for deserialization dispatch.
    message_type TEXT NOT NULL,

    -- payload contains the TLV-encoded message data.
    payload BLOB NOT NULL,

    -- promise_id is set for Ask messages to track the response.
    -- NULL for Tell (fire-and-forget) messages.
    promise_id TEXT,

    -- callback_actor_id is set for DurableAsk messages to route the response.
    -- The response will be delivered to this actor's mailbox via outbox.
    -- NULL for regular Ask/Tell messages.
    callback_actor_id TEXT,

    -- correlation_id links DurableAsk requests to their responses.
    -- The response message will include this ID for matching.
    -- NULL for regular Ask/Tell messages.
    correlation_id TEXT,

    -- priority determines processing order (higher = more important).
    -- Used for restart messages which need front-of-queue processing.
    priority INTEGER NOT NULL DEFAULT 0,

    -- Lease management fields.
    -- lease_token is an opaque token that must match for Ack/Nack to succeed.
    -- This prevents stale acks from a previous lease holder after crash.
    lease_token TEXT,

    -- lease_until is the unix timestamp when the lease expires.
    -- After expiry, the message becomes available for redelivery.
    lease_until INTEGER,

    -- Delivery tracking fields.
    -- available_at is the unix timestamp when the message becomes available.
    -- Used for scheduling initial delivery and retry delays after Nack.
    available_at INTEGER NOT NULL,

    -- attempts tracks how many times delivery has been attempted.
    attempts INTEGER NOT NULL DEFAULT 0,

    -- max_attempts is the maximum delivery attempts before dead-lettering.
    max_attempts INTEGER NOT NULL DEFAULT 10,

    -- created_at is the unix timestamp when the message was enqueued.
    created_at INTEGER NOT NULL
);

-- Index for efficient polling of available messages.
-- Covers: mailbox lookup, availability check, priority ordering.
-- Note: We cannot use a partial index with strftime() since it's non-deterministic.
-- The query handles lease expiry filtering at runtime.
CREATE INDEX IF NOT EXISTS idx_mailbox_messages_available
    ON mailbox_messages(mailbox_id, available_at, priority DESC);

-- Index for lease expiry cleanup.
CREATE INDEX IF NOT EXISTS idx_mailbox_messages_lease
    ON mailbox_messages(lease_until)
    WHERE lease_until IS NOT NULL;

-- Index for promise lookups (Ask result retrieval).
CREATE INDEX IF NOT EXISTS idx_mailbox_messages_promise
    ON mailbox_messages(promise_id)
    WHERE promise_id IS NOT NULL;

-- Ask results table.
-- Persists results for Ask messages so callers can recover outcomes after crash.
-- Separating this from mailbox_messages allows the original message to be deleted
-- while the result remains available for the caller.
CREATE TABLE IF NOT EXISTS ask_results (
    -- promise_id links to the original Ask message.
    promise_id TEXT PRIMARY KEY,

    -- result_blob contains the TLV-encoded successful result.
    -- NULL if the request failed with an error.
    result_blob BLOB,

    -- error_text contains the error message if the request failed.
    -- NULL if the request succeeded.
    error_text TEXT,

    -- created_at is the unix timestamp when the result was persisted.
    created_at INTEGER NOT NULL,

    -- expires_at is the unix timestamp after which this result can be garbage
    -- collected. Callers should retrieve results before expiry.
    expires_at INTEGER NOT NULL
);

-- Index for TTL-based cleanup of expired results.
CREATE INDEX IF NOT EXISTS idx_ask_results_expires
    ON ask_results(expires_at);

-- Transactional outbox table.
-- Messages destined for other actors are written here in the same transaction
-- as FSM state changes. A background publisher drains this table and delivers
-- messages, only deleting after successful delivery. This implements CDC.
CREATE TABLE IF NOT EXISTS outbox_messages (
    -- id is a ULID providing time-ordering and uniqueness.
    id TEXT PRIMARY KEY,

    -- source_actor_id identifies the actor that created this message.
    source_actor_id TEXT NOT NULL,

    -- target_actor_id identifies the destination actor's mailbox.
    target_actor_id TEXT NOT NULL,

    -- message_type is the type name for deserialization dispatch.
    message_type TEXT NOT NULL,

    -- payload contains the TLV-encoded message data.
    payload BLOB NOT NULL,

    -- domain_key is an optional natural idempotency key.
    -- For example: "round:abc123:phase:nonces" ensures the same round/phase
    -- combination is only processed once by the receiver.
    domain_key TEXT,

    -- version is a monotonic counter for ordering within a domain.
    -- Higher versions supersede lower versions for the same domain_key.
    version INTEGER NOT NULL DEFAULT 0,

    -- status tracks the delivery lifecycle.
    -- Values: 'pending', 'completed', 'dead_letter'
    status TEXT NOT NULL DEFAULT 'pending',

    -- delivery_attempts tracks how many times delivery was attempted.
    delivery_attempts INTEGER NOT NULL DEFAULT 0,

    -- created_at is the unix timestamp when the message was enqueued.
    created_at INTEGER NOT NULL,

    -- completed_at is the unix timestamp when delivery completed (or failed).
    completed_at INTEGER
);

-- Index for efficient polling of pending outbox messages.
CREATE INDEX IF NOT EXISTS idx_outbox_messages_pending
    ON outbox_messages(status, created_at)
    WHERE status = 'pending';

-- Index for domain key lookups (idempotency checks by receiver).
CREATE INDEX IF NOT EXISTS idx_outbox_messages_domain_key
    ON outbox_messages(domain_key)
    WHERE domain_key IS NOT NULL;

-- Message deduplication table.
-- Tracks message IDs that have been processed to prevent duplicate processing
-- on redelivery. Entries expire after TTL and are garbage collected.
CREATE TABLE IF NOT EXISTS processed_messages (
    -- id is the message ID that was processed.
    id TEXT PRIMARY KEY,

    -- actor_id identifies which actor processed this message.
    actor_id TEXT NOT NULL,

    -- processed_at is the unix timestamp when processing completed.
    processed_at INTEGER NOT NULL,

    -- expires_at is the unix timestamp after which this entry can be deleted.
    -- Should exceed the maximum possible redelivery window.
    expires_at INTEGER NOT NULL
);

-- Index for TTL-based cleanup of expired entries.
CREATE INDEX IF NOT EXISTS idx_processed_messages_expires
    ON processed_messages(expires_at);

-- FSM state checkpoints table.
-- Stores serialized FSM state for crash recovery. On restart, the actor loads
-- the checkpoint and sends a RestartMessage to resume from the saved state.
CREATE TABLE IF NOT EXISTS fsm_checkpoints (
    -- actor_id identifies the actor whose FSM state is checkpointed.
    actor_id TEXT PRIMARY KEY,

    -- state_type is the name of the current FSM state for quick lookup.
    state_type TEXT NOT NULL,

    -- state_data contains the TLV-encoded state snapshot.
    state_data BLOB NOT NULL,

    -- version is a monotonic counter incremented on each checkpoint.
    -- Used for conflict detection and debugging.
    version INTEGER NOT NULL DEFAULT 0,

    -- updated_at is the unix timestamp of the last checkpoint.
    updated_at INTEGER NOT NULL
);

-- Dead letter queue table.
-- Stores messages that failed after max_attempts or had unrecoverable errors.
-- Useful for debugging and manual intervention.
CREATE TABLE IF NOT EXISTS dead_letters (
    -- id is the original message ID.
    id TEXT PRIMARY KEY,

    -- source indicates where the message originated: 'mailbox' or 'outbox'.
    source TEXT NOT NULL,

    -- actor_id identifies the target actor (for mailbox) or source (for outbox).
    actor_id TEXT NOT NULL,

    -- message_type is the type name for the failed message.
    message_type TEXT NOT NULL,

    -- payload contains the original TLV-encoded message data.
    payload BLOB NOT NULL,

    -- failure_reason describes why the message was dead-lettered.
    failure_reason TEXT NOT NULL,

    -- attempts is the number of delivery attempts before dead-lettering.
    attempts INTEGER NOT NULL,

    -- created_at is the unix timestamp when the message was dead-lettered.
    created_at INTEGER NOT NULL
);

-- Index for querying dead letters by actor.
CREATE INDEX IF NOT EXISTS idx_dead_letters_actor
    ON dead_letters(actor_id, created_at DESC);

-- Index for querying dead letters by source type.
CREATE INDEX IF NOT EXISTS idx_dead_letters_source
    ON dead_letters(source, created_at DESC);
