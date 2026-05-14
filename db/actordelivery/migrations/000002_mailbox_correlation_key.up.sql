-- Per-correlation-key FIFO claim ordering for the durable actor mailbox.
--
-- The default claim path orders by (priority DESC, available_at ASC,
-- created_at ASC). Under transient Tell failures the framework Nacks a
-- message with a backoff delay (available_at = now + delay). A
-- later-enqueued message in the same logical sequence can have a smaller
-- available_at than the in-backoff message and claim before it,
-- producing observable out-of-order processing for downstream consumers.
--
-- This migration adds an optional correlation_key column. The claim
-- query is updated separately (in mailbox.sql) to refuse to return a
-- keyed row when an earlier same-key row is still in the mailbox, even
-- if the earlier row is in backoff. Unkeyed messages (NULL key) keep
-- the existing global available_at ordering and are unaffected by
-- keyed lanes.
--
-- The column is nullable with no backfill: pre-upgrade in-flight rows
-- read back as NULL and continue to participate in the global ordering.
-- New enqueues stamp the key from the message's CorrelationKey() at
-- enqueue time.

ALTER TABLE mailbox_messages ADD COLUMN correlation_key TEXT;

-- Filtered composite index supports the anti-join in the claim query.
-- The NOT EXISTS subquery probes for an earlier same-key row (by id)
-- scoped to the same mailbox; this index makes that probe an index
-- seek. We index by id rather than created_at because the claim's
-- per-key FIFO uses the UUIDv7 id column for strict sub-second
-- ordering (the id encodes millisecond timestamp + tiebreaker bits).
CREATE INDEX IF NOT EXISTS idx_mailbox_messages_correlation
    ON mailbox_messages(mailbox_id, correlation_key, id)
    WHERE correlation_key IS NOT NULL;
