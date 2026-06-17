-- Durable mailbox queries.
-- These queries support lease-based message delivery with exactly-once semantics.

-- =============================================================================
-- Mailbox Message Operations
-- =============================================================================

-- name: EnqueueMailboxMessage :exec
-- Enqueue a new message to an actor's mailbox.
-- ON CONFLICT (id) DO NOTHING enables receiver-side deduplication for outbox
-- delivery: if the OutboxPublisher successfully delivers a message but the
-- subsequent CompleteOutbox call fails, the retry will attempt to insert the
-- same outbox-derived ID. The conflict clause makes this a silent no-op
-- instead of an error, preserving exactly-once inbox semantics.
-- correlation_key is optional (NULL = unkeyed, participates in the global
-- available_at order). Non-NULL keys participate in per-key FIFO claim
-- ordering: see LeaseNextMailboxMessage for the head-of-line rule.
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
    created_at,
    correlation_key
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
ON CONFLICT (id) DO NOTHING;

-- name: LeaseNextMailboxMessage :one
-- Atomically claim the next available message for processing.
-- Sets lease_token and lease_until, increments attempts.
-- Returns NULL if no messages are available.
--
-- Ordering: priority DESC ensures high-priority (e.g., restart) messages
-- first, then available_at ASC for delivery order, then created_at ASC as
-- a tiebreaker to ensure deterministic ordering when priority and
-- available_at are equal.
--
-- Per-correlation-key FIFO: when a row carries a non-NULL correlation_key,
-- it is eligible only if no earlier same-key row exists in this mailbox.
-- "Earlier" is determined by the UUIDv7 id column, which embeds a
-- millisecond timestamp plus a per-generator tiebreaker so two messages
-- enqueued by the same producer are always strictly orderable, even
-- when they fall in the same second-granularity created_at bucket. This
-- prevents a later-enqueued same-key message from overtaking a same-key
-- message that is currently in retry backoff (available_at pushed into
-- the future by a Nack). Unkeyed rows (NULL key) skip the anti-join
-- and participate in the global available_at order as before; they are
-- not affected by, and do not affect, keyed lanes.
--
-- The anti-join also requires the predecessor to still have retry budget
-- (m2.attempts < m2.max_attempts). Without this clause, a same-key row
-- that exhausted its attempts but has not yet been physically deleted
-- (e.g. a crash window between MoveMailboxToDeadLetter and
-- DeleteMailboxMessage in handlePoisonMessage) would permanently block
-- every later same-key message instead of being passed over. The
-- exhausted row is already filtered out of the outer candidate set by
-- m.attempts < m.max_attempts, so this just brings the anti-join
-- predicate into agreement with the eligibility predicate.
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
      AND (
          m.correlation_key IS NULL
          OR NOT EXISTS (
              SELECT 1 FROM mailbox_messages m2
              WHERE m2.mailbox_id = m.mailbox_id
                AND m2.correlation_key = m.correlation_key
                AND m2.id < m.id
                AND m2.attempts < m2.max_attempts
          )
      )
    ORDER BY m.priority DESC, m.available_at ASC, m.created_at ASC
    LIMIT 1
)
RETURNING *;

-- name: PeekNextMailboxMessage :one
-- Read-only claim of the next available message WITHOUT taking a lease.
-- This is the leaseless fast path for single-worker (NumWorkers == 1) actors:
-- with no competing consumer, the lease_token's only purpose -- fencing the
-- ack against another worker -- is unnecessary, so the extra write transaction
-- that LeaseNextMailboxMessage performs (set lease_token/lease_until, bump
-- attempts) is pure overhead. Peek runs as a pure SELECT, so a crash between
-- peek and the consuming Commit leaves the message untouched and it is simply
-- re-peeked on restart (at-least-once preserved, identical to today's
-- lease-expiry redelivery).
--
-- The eligibility predicate and ORDER BY mirror LeaseNextMailboxMessage's inner
-- SELECT EXACTLY so the leaseless path observes the same per-correlation-key
-- FIFO and priority/available_at/created_at ordering. See that query for the
-- full rationale on the correlation-key anti-join and the m2.attempts <
-- m2.max_attempts predicate.
--
-- Because peek takes no lease, attempts is NOT incremented here. The store
-- adapter maps the returned row to an empty-token delivery even if the
-- persisted row has stale metadata from an expired lease. That empty token is
-- part of the framework contract: it routes the actor layer to the by-ID
-- ack/nack path. The single-worker consume path increments attempts on its
-- failure (nack) path via NackMailboxMessageByID so a repeatedly-failing
-- message still climbs to max_attempts and dead-letters.
SELECT m.* FROM mailbox_messages m
WHERE m.mailbox_id = $1
  AND m.available_at <= $2
  AND (m.lease_until IS NULL OR m.lease_until < $2)
  AND m.attempts < m.max_attempts
  AND (
      m.correlation_key IS NULL
      OR NOT EXISTS (
          SELECT 1 FROM mailbox_messages m2
          WHERE m2.mailbox_id = m.mailbox_id
            AND m2.correlation_key = m.correlation_key
            AND m2.id < m.id
            AND m2.attempts < m2.max_attempts
      )
  )
ORDER BY m.priority DESC, m.available_at ASC, m.created_at ASC
LIMIT 1;

-- name: AckMailboxMessage :execrows
-- Acknowledge successful processing. Deletes the message.
-- Validates lease_token to prevent stale acks.
DELETE FROM mailbox_messages
WHERE id = $1 AND lease_token = $2;

-- name: AckMailboxMessageByID :execrows
-- Unfenced acknowledgment used by the leaseless single-worker consume path.
-- Deletes the message by ID without validating a lease_token, because a
-- single-worker actor has no competing consumer to fence against. Folded into
-- the behavior's Commit transaction, so a crash before commit leaves the row
-- intact for re-peek. MUST NOT be used by the multi-worker (NumWorkers > 1)
-- path, which relies on lease_token fencing via AckMailboxMessage.
DELETE FROM mailbox_messages
WHERE id = $1;

-- name: NackMailboxMessageByID :execrows
-- Unfenced redelivery release for the leaseless single-worker consume path.
-- Sets a new available_at and increments attempts. The attempts bump is
-- essential: the leaseless peek does not increment attempts (unlike a lease),
-- so the failure path must do it here, otherwise a repeatedly-failing message
-- would never climb to max_attempts and dead-letter. No lease_token clause,
-- because a single-worker actor has no competing consumer to fence against.
-- Any stale expired lease metadata is cleared so the persisted row matches the
-- leaseless state machine after a retry decision.
UPDATE mailbox_messages
SET
    lease_token = NULL,
    lease_until = NULL,
    available_at = $2,
    attempts = attempts + 1
WHERE id = $1;

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
ORDER BY priority DESC, available_at ASC, created_at ASC;

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
-- Claim a batch of pending outbox messages for delivery. Sets a claim token
-- and expiry to prevent concurrent publishers from processing the same messages.
-- Only selects rows that are unclaimed or whose claim has expired.
UPDATE outbox_messages
SET delivery_attempts = delivery_attempts + 1,
    claim_token = $2,
    claimed_until = $3
WHERE id IN (
    SELECT o.id FROM outbox_messages o
    WHERE o.status = 'pending'
      AND (o.claimed_until IS NULL OR o.claimed_until < $4)
    ORDER BY o.created_at ASC
    LIMIT $1
)
RETURNING *;

-- name: CompleteOutboxMessage :exec
-- Mark an outbox message as successfully delivered. The claim token must match
-- to prevent stale publishers from completing messages they no longer own.
UPDATE outbox_messages
SET status = 'completed', completed_at = $2
WHERE id = $1 AND claim_token = $3;

-- name: FailOutboxMessage :exec
-- Mark an outbox message as failed (dead letter). The claim token must match
-- to prevent stale publishers from failing messages they no longer own.
UPDATE outbox_messages
SET status = 'dead_letter', completed_at = $2
WHERE id = $1 AND claim_token = $3;

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

-- NOTE: DeleteOutboxMessage and CleanupCompletedOutbox are intentionally
-- omitted from this file. A dedicated GC procedure will be added in a
-- follow-up to handle cleanup of completed outbox messages and dead letters
-- with configurable retention policies.

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
