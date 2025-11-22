-- Rollback boarding tables migration.
-- This removes the boarding_intents, boarding_addresses, and boarding_statuses tables.

DROP INDEX IF EXISTS idx_boarding_intents_conf_height;
DROP INDEX IF EXISTS idx_boarding_intents_status;
DROP INDEX IF EXISTS idx_boarding_intents_pk_script;
DROP TABLE IF EXISTS boarding_intents;

DROP INDEX IF EXISTS idx_boarding_addresses_last_confirmed;
DROP TABLE IF EXISTS boarding_addresses;

DROP TABLE IF EXISTS boarding_statuses;
