-- Rollback VTXO tables migration.
-- Drop indexes and tables in reverse dependency order.

-- Drop forfeit info indexes and table.
DROP INDEX IF EXISTS idx_forfeit_infos_round;
DROP TABLE IF EXISTS round_forfeit_infos;

-- Drop VTXOs indexes and table.
DROP INDEX IF EXISTS idx_vtxos_locked;
DROP INDEX IF EXISTS idx_vtxos_status;
DROP INDEX IF EXISTS idx_vtxos_round;
DROP TABLE IF EXISTS vtxos;
DROP TABLE IF EXISTS vtxo_statuses;
