-- Roll back the correlation-key FIFO claim ordering migration.

DROP INDEX IF EXISTS idx_mailbox_messages_correlation;

ALTER TABLE mailbox_messages DROP COLUMN correlation_key;
