-- Dead-letter requeue support.
--
-- The original dead_letters projection dropped every routing field of the
-- source mailbox message (priority, retry budget, ask plumbing, and the
-- per-key FIFO tag), which made a faithful requeue impossible: a dead letter
-- could be inspected but never reconstructed as a mailbox message. These
-- columns carry the full routing identity forward so an operator-driven
-- requeue can re-enqueue the message exactly as it was originally sent.
--
-- All columns are nullable or defaulted so rows dead-lettered before this
-- migration remain readable; a requeue of such a legacy row falls back to
-- the same defaults a fresh enqueue would get.

-- promise_id is the ask-result key for Ask messages (NULL for Tell).
ALTER TABLE dead_letters ADD COLUMN promise_id TEXT;

-- callback_actor_id routes DurableAsk responses (NULL otherwise).
ALTER TABLE dead_letters ADD COLUMN callback_actor_id TEXT;

-- correlation_id links DurableAsk requests to responses (NULL otherwise).
ALTER TABLE dead_letters ADD COLUMN correlation_id TEXT;

-- correlation_key is the per-message FIFO lane tag (NULL when unkeyed).
ALTER TABLE dead_letters ADD COLUMN correlation_key TEXT;

-- priority is the mailbox processing priority of the original message.
ALTER TABLE dead_letters ADD COLUMN priority INTEGER NOT NULL DEFAULT 0;

-- max_attempts is the retry budget the original message carried.
ALTER TABLE dead_letters ADD COLUMN max_attempts INTEGER NOT NULL DEFAULT 10;
