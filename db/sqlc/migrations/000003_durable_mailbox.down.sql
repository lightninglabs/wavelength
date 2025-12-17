-- Rollback durable mailbox migration.
-- This removes all tables and indexes created for durable actor mailboxes.

-- Drop dead letters table and indexes.
DROP INDEX IF EXISTS idx_dead_letters_source;
DROP INDEX IF EXISTS idx_dead_letters_actor;
DROP TABLE IF EXISTS dead_letters;

-- Drop FSM checkpoints table.
DROP TABLE IF EXISTS fsm_checkpoints;

-- Drop processed messages table and index.
DROP INDEX IF EXISTS idx_processed_messages_expires;
DROP TABLE IF EXISTS processed_messages;

-- Drop outbox messages table and indexes.
DROP INDEX IF EXISTS idx_outbox_messages_domain_key;
DROP INDEX IF EXISTS idx_outbox_messages_pending;
DROP TABLE IF EXISTS outbox_messages;

-- Drop ask results table and index.
DROP INDEX IF EXISTS idx_ask_results_expires;
DROP TABLE IF EXISTS ask_results;

-- Drop mailbox messages table and indexes.
DROP INDEX IF EXISTS idx_mailbox_messages_promise;
DROP INDEX IF EXISTS idx_mailbox_messages_lease;
DROP INDEX IF EXISTS idx_mailbox_messages_available;
DROP TABLE IF EXISTS mailbox_messages;
