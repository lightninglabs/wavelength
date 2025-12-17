-- Durable mailbox queries.
-- These queries support lease-based message delivery with exactly-once semantics.

-- =============================================================================
-- Mailbox Message Operations
-- =============================================================================

-- name: EnqueueMailboxMessage :exec
-- Enqueue a new message to an actor's mailbox.
INSERT INTO mailbox_messages (
    id,
    mailbox_id,
    message_type,
    payload,
    promise_id,
    callback_actor_id,
    correlation_id,
    priority,
    available_at,
    max_attempts,
    created_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11);

-- name: LeaseNextMailboxMessage :one
-- Atomically claim the next available message for processing.
-- Sets lease_token and lease_until, increments attempts.
-- Returns NULL if no messages are available.
UPDATE mailbox_messages
SET
    lease_token = $2,
    lease_until = $3,
    attempts = attempts + 1
WHERE mailbox_messages.id = (
    SELECT m.id FROM mailbox_messages m
    WHERE m.mailbox_id = $1
      AND m.available_at <= $4
      AND (m.lease_until IS NULL OR m.lease_until < $4)
      AND m.attempts < m.max_attempts
    ORDER BY m.priority DESC, m.available_at ASC
    LIMIT 1
)
RETURNING *;

-- name: AckMailboxMessage :execrows
-- Acknowledge successful processing. Deletes the message.
-- Validates lease_token to prevent stale acks.
DELETE FROM mailbox_messages
WHERE id = $1 AND lease_token = $2;

-- name: NackMailboxMessage :execrows
-- Release message for redelivery after retry delay.
-- Clears lease and sets new available_at.
-- Validates lease_token to prevent stale nacks.
UPDATE mailbox_messages
SET
    lease_token = NULL,
    lease_until = NULL,
    available_at = $3
WHERE id = $1 AND lease_token = $2;

-- name: ExtendMailboxLease :execrows
-- Extend the lease for long-running message processing.
-- Validates lease_token to prevent stale extensions.
UPDATE mailbox_messages
SET lease_until = $3
WHERE id = $1 AND lease_token = $2;

-- name: GetMailboxMessage :one
-- Get a specific mailbox message by ID.
SELECT * FROM mailbox_messages WHERE id = $1;

-- name: CountPendingMailboxMessages :one
-- Count pending messages for an actor's mailbox.
SELECT COUNT(*) FROM mailbox_messages
WHERE mailbox_id = $1
  AND (lease_until IS NULL OR lease_until < $2);

-- name: ExpireMailboxLeases :exec
-- Release all expired leases so messages can be redelivered.
-- Called periodically by a background cleanup task.
UPDATE mailbox_messages
SET
    lease_token = NULL,
    lease_until = NULL
WHERE lease_until IS NOT NULL AND lease_until < $1;

-- name: MoveMailboxToDeadLetter :exec
-- Move a failed message to the dead letter queue.
INSERT INTO dead_letters (id, source, actor_id, message_type, payload, failure_reason, attempts, created_at)
SELECT m.id, 'mailbox', m.mailbox_id, m.message_type, m.payload, $2, m.attempts, $3
FROM mailbox_messages m
WHERE m.id = $1;

-- name: DeleteMailboxMessage :exec
-- Delete a mailbox message by ID (used after moving to dead letter).
DELETE FROM mailbox_messages WHERE id = $1;

-- name: ListMailboxMessagesByActor :many
-- List all messages for an actor's mailbox (for debugging).
SELECT * FROM mailbox_messages
WHERE mailbox_id = $1
ORDER BY priority DESC, available_at ASC;

-- =============================================================================
-- Ask Result Operations
-- =============================================================================

-- name: InsertAskResult :exec
-- Store the result of an Ask message for caller retrieval.
INSERT INTO ask_results (promise_id, result_blob, error_text, created_at, expires_at)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (promise_id) DO NOTHING;

-- name: GetAskResult :one
-- Retrieve the result of an Ask message.
SELECT * FROM ask_results WHERE promise_id = $1;

-- name: DeleteAskResult :exec
-- Delete an Ask result after retrieval.
DELETE FROM ask_results WHERE promise_id = $1;

-- name: CleanupExpiredAskResults :exec
-- Delete Ask results that have expired.
DELETE FROM ask_results WHERE expires_at < $1;

-- =============================================================================
-- Outbox Operations (CDC Pattern)
-- =============================================================================

-- name: EnqueueOutboxMessage :exec
-- Enqueue a message to the transactional outbox.
-- Called within the same transaction as FSM state changes.
INSERT INTO outbox_messages (
    id,
    source_actor_id,
    target_actor_id,
    message_type,
    payload,
    domain_key,
    version,
    created_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8);

-- name: ClaimOutboxBatch :many
-- Claim a batch of pending outbox messages for delivery.
-- Updates status to 'pending' with incremented delivery_attempts.
-- Returns messages ordered by creation time.
UPDATE outbox_messages
SET delivery_attempts = delivery_attempts + 1
WHERE id IN (
    SELECT id FROM outbox_messages
    WHERE status = 'pending'
    ORDER BY created_at ASC
    LIMIT $1
)
RETURNING *;

-- name: CompleteOutboxMessage :exec
-- Mark an outbox message as successfully delivered.
UPDATE outbox_messages
SET status = 'completed', completed_at = $2
WHERE id = $1;

-- name: FailOutboxMessage :exec
-- Mark an outbox message as failed (dead letter).
UPDATE outbox_messages
SET status = 'dead_letter', completed_at = $2
WHERE id = $1;

-- name: GetOutboxMessage :one
-- Get a specific outbox message by ID.
SELECT * FROM outbox_messages WHERE id = $1;

-- name: CountPendingOutboxMessages :one
-- Count pending outbox messages.
SELECT COUNT(*) FROM outbox_messages WHERE status = 'pending';

-- name: ListPendingOutboxByTarget :many
-- List pending outbox messages for a specific target actor.
SELECT * FROM outbox_messages
WHERE target_actor_id = $1 AND status = 'pending'
ORDER BY created_at ASC;

-- name: MoveOutboxToDeadLetter :exec
-- Move a failed outbox message to the dead letter queue.
INSERT INTO dead_letters (id, source, actor_id, message_type, payload, failure_reason, attempts, created_at)
SELECT o.id, 'outbox', o.source_actor_id, o.message_type, o.payload, $2, o.delivery_attempts, $3
FROM outbox_messages o
WHERE o.id = $1;

-- name: DeleteOutboxMessage :exec
-- Delete an outbox message by ID (cleanup after completion or dead letter).
DELETE FROM outbox_messages WHERE id = $1;

-- name: CleanupCompletedOutbox :exec
-- Delete completed outbox messages older than a threshold.
DELETE FROM outbox_messages
WHERE status = 'completed' AND completed_at < $1;

-- =============================================================================
-- Processed Messages (Deduplication)
-- =============================================================================

-- name: MarkMessageProcessed :exec
-- Record that a message has been processed for deduplication.
INSERT INTO processed_messages (id, actor_id, processed_at, expires_at)
VALUES ($1, $2, $3, $4)
ON CONFLICT (id) DO NOTHING;

-- name: IsMessageProcessed :one
-- Check if a message has already been processed.
SELECT EXISTS(SELECT 1 FROM processed_messages WHERE id = $1) AS processed;

-- name: CleanupExpiredProcessedMessages :exec
-- Delete expired deduplication entries.
DELETE FROM processed_messages WHERE expires_at < $1;

-- =============================================================================
-- FSM Checkpoints
-- =============================================================================

-- name: SaveFSMCheckpoint :exec
-- Save or update an FSM state checkpoint.
INSERT INTO fsm_checkpoints (actor_id, state_type, state_data, version, updated_at)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (actor_id) DO UPDATE
SET state_type = excluded.state_type,
    state_data = excluded.state_data,
    version = excluded.version,
    updated_at = excluded.updated_at;

-- name: GetFSMCheckpoint :one
-- Load an FSM checkpoint for an actor.
SELECT * FROM fsm_checkpoints WHERE actor_id = $1;

-- name: DeleteFSMCheckpoint :exec
-- Delete an FSM checkpoint (e.g., when actor terminates normally).
DELETE FROM fsm_checkpoints WHERE actor_id = $1;

-- name: ListFSMCheckpoints :many
-- List all FSM checkpoints (for debugging/admin).
SELECT * FROM fsm_checkpoints ORDER BY updated_at DESC;

-- =============================================================================
-- Dead Letter Operations
-- =============================================================================

-- name: GetDeadLetter :one
-- Get a specific dead letter by ID.
SELECT * FROM dead_letters WHERE id = $1;

-- name: ListDeadLettersByActor :many
-- List dead letters for a specific actor.
SELECT * FROM dead_letters
WHERE actor_id = $1
ORDER BY created_at DESC
LIMIT $2;

-- name: ListDeadLettersBySource :many
-- List dead letters by source type (mailbox or outbox).
SELECT * FROM dead_letters
WHERE source = $1
ORDER BY created_at DESC
LIMIT $2;

-- name: DeleteDeadLetter :exec
-- Delete a dead letter after manual processing.
DELETE FROM dead_letters WHERE id = $1;

-- name: CountDeadLetters :one
-- Count total dead letters.
SELECT COUNT(*) FROM dead_letters;

-- name: CleanupOldDeadLetters :exec
-- Delete dead letters older than a threshold.
DELETE FROM dead_letters WHERE created_at < $1;
