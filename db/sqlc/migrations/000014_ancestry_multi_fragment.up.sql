-- Drop the per-commitment UNIQUE constraint on vtxo_ancestry_paths.
--
-- The original schema required distinct rows for one VTXO to carry
-- distinct commitment_txids. That contract is wrong: an OOR spend whose
-- inputs sit at different leaves of the same commitment tree carries one
-- ancestry fragment per leaf, each anchored at the SAME commitment tx,
-- because each leaf needs its own root-to-leaf path for unilateral exit.
-- The operator's indexer groups ancestry the same way (one AncestryPath
-- per batch-tree path within a commitment), so the old constraint made
-- such VTXOs unpersistable and stranded incoming transfers whose inputs
-- shared a round.
--
-- Fragment identity is now (commitment_txid, tree_path); exact
-- duplicates are rejected in Go (upsertAncestryPaths) rather than at the
-- schema level, since a UNIQUE index over the potentially-large tree_path
-- blob would not be portable across SQLite and Postgres.
--
-- SQLite cannot drop a table constraint in place, so rebuild the table
-- and copy the rows. The column set matches 000004_vtxos plus the
-- commitment_height column added in 000013; the per-column docs are
-- carried over (and updated where the contract changed) so the final
-- generated schema keeps them.
--
-- vtxo_ancestry_paths is a one-to-many side table keyed by VTXO
-- outpoint. ON DELETE CASCADE keeps the side table consistent if the
-- VTXO row is ever deleted.
CREATE TABLE vtxo_ancestry_paths_new (
    -- vtxo_outpoint_hash and vtxo_outpoint_index identify the parent VTXO
    -- in the vtxos table.
    vtxo_outpoint_hash BLOB NOT NULL,
    vtxo_outpoint_index INTEGER NOT NULL,

    -- path_order is the deterministic ordinal of this fragment within
    -- the parent VTXO's ancestry, starting at 0. Persists the order
    -- chosen by the indexer (typically grouped by commitment_txid) so
    -- the unroller's broadcast plan is reproducible across restarts.
    path_order INTEGER NOT NULL,

    -- commitment_txid is the 32-byte commitment tx hash anchoring this
    -- fragment. Rows for one VTXO may share a commitment_txid: an OOR
    -- spend whose inputs sat at different leaves of one commitment tree
    -- persists one row per leaf. Fragment identity is the
    -- (commitment_txid, tree_path) pair, enforced in Go
    -- (upsertAncestryPaths).
    commitment_txid BLOB NOT NULL,

    -- tree_path is the TLV-encoded extracted tree.Tree fragment from the
    -- batch root to the input VTXO leaf served by this fragment.
    tree_path BLOB NOT NULL,

    -- tree_depth is the depth of the served leaf within this fragment's
    -- tree. Worst-case unilateral-exit timing for the parent VTXO is
    -- max(tree_depth) across all fragments.
    tree_depth INTEGER NOT NULL,

    -- input_indices is a length-prefixed BE-uint32 list of Ark tx input
    -- indices (within the OOR Ark tx that produced the parent VTXO)
    -- that this fragment serves. Empty for round-direct VTXOs.
    --
    -- No SQL-level DEFAULT here: INSERT statements always pass an
    -- explicit value (empty length-prefixed slice for round-direct
    -- rows). A `DEFAULT X''` literal works on SQLite but is parsed by
    -- Postgres as a bit-string and rejected against the BYTEA column.
    input_indices BLOB NOT NULL,

    -- commitment_height is the on-chain confirmation height of the
    -- commitment tx anchoring this ancestry fragment. It is the tightest
    -- sound floor for the unroller's proof-node confirmation-watch
    -- height hint (nothing in a VTXO's proof graph confirms before its
    -- commitment tx). DEFAULT 0 means unknown: rows persisted before the
    -- column existed, and rows whose producer did not populate it, read
    -- back as 0 and make the unroller fall back to a bounded lookback
    -- floor.
    commitment_height INTEGER NOT NULL DEFAULT 0,

    PRIMARY KEY (vtxo_outpoint_hash, vtxo_outpoint_index, path_order),
    FOREIGN KEY (vtxo_outpoint_hash, vtxo_outpoint_index)
        REFERENCES vtxos(outpoint_hash, outpoint_index)
        ON DELETE CASCADE,

    -- path_order must be a small non-negative ordinal. The active
    -- fragment-count cap (MaxAncestryFragments) is well under 64;
    -- this CHECK guards against a caller persisting a row at a
    -- nonsense ordinal (e.g. negative, or a uint32 round-trip from
    -- malformed wire data) without coupling the schema to the exact
    -- runtime cap.
    CHECK (path_order >= 0 AND path_order < 64)
);

INSERT INTO vtxo_ancestry_paths_new (
    vtxo_outpoint_hash, vtxo_outpoint_index, path_order, commitment_txid,
    tree_path, tree_depth, input_indices, commitment_height
)
SELECT
    vtxo_outpoint_hash, vtxo_outpoint_index, path_order, commitment_txid,
    tree_path, tree_depth, input_indices, commitment_height
FROM vtxo_ancestry_paths;

DROP TABLE vtxo_ancestry_paths;

ALTER TABLE vtxo_ancestry_paths_new RENAME TO vtxo_ancestry_paths;

-- Recreate the lookup index for the common path-by-vtxo query in the
-- unroller; it was dropped together with the old table.
CREATE INDEX idx_vtxo_ancestry_paths_vtxo
    ON vtxo_ancestry_paths(vtxo_outpoint_hash, vtxo_outpoint_index);
