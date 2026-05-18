-- Round tables down migration.
-- Drops all tables created in the up migration in reverse order.

DROP INDEX IF EXISTS idx_vtxos_status;
DROP INDEX IF EXISTS idx_vtxos_creation_time;
DROP INDEX IF EXISTS idx_vtxos_spent;
DROP INDEX IF EXISTS idx_vtxos_round_id;
DROP TABLE IF EXISTS vtxos;

DROP INDEX IF EXISTS idx_client_round_effects_due;
DROP INDEX IF EXISTS idx_client_round_effects_round;
DROP TABLE IF EXISTS client_round_effects;
DROP TABLE IF EXISTS client_round_pending_leave_quotes;
DROP TABLE IF EXISTS client_round_pending_vtxo_quotes;
DROP INDEX IF EXISTS idx_client_round_pending_quotes_created;
DROP TABLE IF EXISTS client_round_pending_quotes;
DROP INDEX IF EXISTS idx_client_round_forfeit_request_state_round_id;
DROP TABLE IF EXISTS client_round_forfeit_request_state;
DROP INDEX IF EXISTS idx_client_round_forfeit_sig_state_round_id;
DROP TABLE IF EXISTS client_round_forfeit_sig_state;
DROP INDEX IF EXISTS idx_client_round_partial_sig_state_round_id;
DROP TABLE IF EXISTS client_round_partial_sig_state;
DROP INDEX IF EXISTS idx_client_round_agg_nonce_state_round_id;
DROP TABLE IF EXISTS client_round_agg_nonce_state;
DROP INDEX IF EXISTS idx_client_round_nonce_state_round_id;
DROP TABLE IF EXISTS client_round_nonce_state;

DROP INDEX IF EXISTS idx_client_tree_txids_tree;
DROP INDEX IF EXISTS idx_client_tree_txids_txid;
DROP TABLE IF EXISTS client_tree_txids;

DROP TABLE IF EXISTS round_client_trees;

DROP TABLE IF EXISTS round_vtxo_requests;

DROP INDEX IF EXISTS idx_round_boarding_intents_round_id;
DROP TABLE IF EXISTS round_boarding_intents;

DROP INDEX IF EXISTS idx_rounds_creation_time;
DROP INDEX IF EXISTS idx_rounds_status;
DROP INDEX IF EXISTS idx_rounds_commitment_txid;
DROP TABLE IF EXISTS rounds;

DROP TABLE IF EXISTS round_statuses;
