-- Revert 000013_round_attribution.

DROP INDEX IF EXISTS idx_round_connector_outputs_round;

DROP TABLE IF EXISTS round_connector_outputs;

ALTER TABLE rounds DROP COLUMN change_output_idx;
