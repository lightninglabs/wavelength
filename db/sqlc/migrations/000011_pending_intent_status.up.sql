-- Add a terminal-failure status to the pending-intent outbox. Until now a
-- pending intent had exactly two fates: adopted by a round (row cleared in the
-- CommitState transaction) or replayed on restart. A round that fails
-- terminally — e.g. the operator cannot fund the commitment tx — had no way to
-- record that the originating job is dead, so the intent replayed forever into
-- the same wall and the user's activity entry stayed pending indefinitely.
--
-- A 'failed' status lets the daemon durably retire such an intent: the replayer
-- skips it (no more replay) and the activity projection surfaces it as FAILED
-- with the operator-supplied reason instead of leaving it stuck pending. The
-- columns live on the kind-agnostic header so both Board and SendOnChain
-- intents share one status vocabulary.
ALTER TABLE pending_intents
    ADD COLUMN status TEXT NOT NULL DEFAULT 'pending';

-- failure_reason is the human-readable failure description, surfaced on the
-- originating job's activity entry. NULL while status = 'pending'.
ALTER TABLE pending_intents
    ADD COLUMN failure_reason TEXT;

-- failure_code is the typed round.RoundFailureCode classification (0 =
-- unknown). Zero while status = 'pending'.
ALTER TABLE pending_intents
    ADD COLUMN failure_code INTEGER NOT NULL DEFAULT 0;

-- Successful intents are deleted the moment a round adopts them, while failed
-- intents are retained indefinitely as durable records, so over time this
-- outbox accumulates only 'failed' rows. Both replay queries scan on
-- kind = ? AND status = 'pending', so a composite index keeps the replayer's
-- startup scan off the growing pile of retained failures.
CREATE INDEX IF NOT EXISTS idx_pending_intents_kind_status
    ON pending_intents (kind, status);
