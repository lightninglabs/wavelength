-- Rollback rounds tables migration.
-- Drop indexes and tables in reverse dependency order.

-- Drop round-related tables.
DROP TABLE IF EXISTS round_client_registrations;
DROP TABLE IF EXISTS round_connector_descriptors;

-- Drop VTXO tree tables and indexes (in reverse dependency order).
DROP INDEX IF EXISTS idx_vtxo_tree_cosigners_key;
DROP TABLE IF EXISTS vtxo_tree_cosigners;
DROP TABLE IF EXISTS vtxo_tree_node_outputs;
DROP INDEX IF EXISTS idx_vtxo_tree_nodes_leaves;
DROP INDEX IF EXISTS idx_vtxo_tree_nodes_depth;
DROP INDEX IF EXISTS idx_vtxo_tree_nodes_parent;
DROP TABLE IF EXISTS vtxo_tree_nodes;
DROP TABLE IF EXISTS round_vtxo_tree;

-- Drop rounds indexes and table.
DROP INDEX IF EXISTS idx_rounds_created_at;
DROP INDEX IF EXISTS idx_rounds_txid;
DROP INDEX IF EXISTS idx_rounds_status;
DROP TABLE IF EXISTS rounds;
DROP TABLE IF EXISTS round_statuses;
