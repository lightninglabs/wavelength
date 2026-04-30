-- Reverse 000009: drop the side table and restore the per-VTXO singular
-- tree_path / tree_depth columns.

DROP INDEX IF EXISTS idx_vtxo_ancestry_paths_vtxo;
DROP TABLE IF EXISTS vtxo_ancestry_paths;

-- Re-add the columns as NULLable: the original schema declared them
-- NOT NULL with a `DEFAULT X''` blob literal, but `X''` is parsed as a
-- bit-string by Postgres and rejected against the BYTEA column. Since
-- this migration is the back-out path for the multi-fragment ancestry
-- shape, callers are expected to repopulate the legacy singular path
-- before relying on it.
ALTER TABLE vtxos ADD COLUMN tree_path BLOB;
ALTER TABLE vtxos ADD COLUMN tree_depth INTEGER;
