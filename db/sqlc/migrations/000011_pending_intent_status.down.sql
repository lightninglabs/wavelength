-- Reverse the pending-intent terminal-failure status columns.
DROP INDEX IF EXISTS idx_pending_intents_kind_status;

ALTER TABLE pending_intents DROP COLUMN failure_code;

ALTER TABLE pending_intents DROP COLUMN failure_reason;

ALTER TABLE pending_intents DROP COLUMN status;
