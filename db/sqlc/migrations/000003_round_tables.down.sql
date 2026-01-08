-- Round tables down migration.
-- Drops all tables created in the up migration in reverse order.

DROP INDEX IF EXISTS idx_vtxos_creation_time;
DROP INDEX IF EXISTS idx_vtxos_spent;
DROP INDEX IF EXISTS idx_vtxos_round_id;
DROP TABLE IF EXISTS vtxos;

DROP TABLE IF EXISTS round_client_trees;

DROP TABLE IF EXISTS round_vtxo_templates;

DROP INDEX IF EXISTS idx_round_boarding_intents_round_id;
DROP TABLE IF EXISTS round_boarding_intents;

DROP INDEX IF EXISTS idx_rounds_creation_time;
DROP INDEX IF EXISTS idx_rounds_status;
DROP INDEX IF EXISTS idx_rounds_commitment_txid;
DROP TABLE IF EXISTS rounds;

DROP TABLE IF EXISTS round_statuses;
