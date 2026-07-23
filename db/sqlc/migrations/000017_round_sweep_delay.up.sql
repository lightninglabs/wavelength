-- The batch-wide CSV delay is an immutable per-round term. Persist it at the
-- point-of-no-return checkpoint so a restarted client registers the batch
-- with the exact delay under which it signed.
ALTER TABLE rounds ADD COLUMN sweep_delay INTEGER NOT NULL DEFAULT 0;
