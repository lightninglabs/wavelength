-- VTXO status tracking down migration.
-- Removes all columns and indexes added in the up migration.

-- Drop index first.
DROP INDEX IF EXISTS idx_vtxos_status;

-- SQLite doesn't support DROP COLUMN directly, so we need to recreate the table.
-- This approach preserves existing data while removing the new columns.

-- Create a temporary table with the original schema.
CREATE TABLE vtxos_backup (
    outpoint_hash BLOB NOT NULL,
    outpoint_index INTEGER NOT NULL,
    round_id TEXT NOT NULL,
    amount BIGINT NOT NULL,
    pk_script BLOB NOT NULL,
    expiry INTEGER NOT NULL,
    client_key_family INTEGER NOT NULL,
    client_key_index INTEGER NOT NULL,
    client_pubkey BLOB NOT NULL,
    operator_pubkey BLOB NOT NULL,
    tree_path BLOB NOT NULL,
    spent BOOLEAN NOT NULL DEFAULT FALSE,
    creation_time BIGINT NOT NULL,
    last_update_time BIGINT NOT NULL,
    PRIMARY KEY (outpoint_hash, outpoint_index),
    FOREIGN KEY (round_id) REFERENCES rounds(round_id)
);

-- Copy data from the current table (excluding new columns).
INSERT INTO vtxos_backup (
    outpoint_hash, outpoint_index, round_id, amount, pk_script, expiry,
    client_key_family, client_key_index, client_pubkey, operator_pubkey,
    tree_path, spent, creation_time, last_update_time
)
SELECT
    outpoint_hash, outpoint_index, round_id, amount, pk_script, expiry,
    client_key_family, client_key_index, client_pubkey, operator_pubkey,
    tree_path, spent, creation_time, last_update_time
FROM vtxos;

-- Drop the modified table.
DROP TABLE vtxos;

-- Rename backup to original name.
ALTER TABLE vtxos_backup RENAME TO vtxos;

-- Recreate original indexes.
CREATE INDEX IF NOT EXISTS idx_vtxos_round_id ON vtxos(round_id);
CREATE INDEX IF NOT EXISTS idx_vtxos_spent ON vtxos(spent);
CREATE INDEX IF NOT EXISTS idx_vtxos_creation_time ON vtxos(creation_time DESC);
