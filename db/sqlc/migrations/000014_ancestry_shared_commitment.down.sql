-- Reverse 000014: rebuild vtxo_ancestry_paths with the original
-- UNIQUE(vtxo_outpoint_hash, vtxo_outpoint_index, commitment_txid) restored.
--
-- NOTE: this down migration fails if any VTXO has since persisted two ancestry
-- fragments that share a commitment_txid (the shared-commitment case that
-- 000014 enables). That is expected: a database that has used the relaxed
-- schema cannot be forced back onto the stricter invariant without dropping
-- data.
CREATE TABLE vtxo_ancestry_paths_old (
    vtxo_outpoint_hash BLOB NOT NULL,
    vtxo_outpoint_index INTEGER NOT NULL,
    path_order INTEGER NOT NULL,
    commitment_txid BLOB NOT NULL,
    tree_path BLOB NOT NULL,
    tree_depth INTEGER NOT NULL,
    input_indices BLOB NOT NULL,
    commitment_height INTEGER NOT NULL DEFAULT 0,

    PRIMARY KEY (vtxo_outpoint_hash, vtxo_outpoint_index, path_order),
    FOREIGN KEY (vtxo_outpoint_hash, vtxo_outpoint_index)
        REFERENCES vtxos(outpoint_hash, outpoint_index)
        ON DELETE CASCADE,

    UNIQUE (vtxo_outpoint_hash, vtxo_outpoint_index, commitment_txid),

    CHECK (path_order >= 0 AND path_order < 64)
);

INSERT INTO vtxo_ancestry_paths_old (
    vtxo_outpoint_hash, vtxo_outpoint_index, path_order, commitment_txid,
    tree_path, tree_depth, input_indices, commitment_height
)
SELECT
    vtxo_outpoint_hash, vtxo_outpoint_index, path_order, commitment_txid,
    tree_path, tree_depth, input_indices, commitment_height
FROM vtxo_ancestry_paths;

DROP TABLE vtxo_ancestry_paths;

ALTER TABLE vtxo_ancestry_paths_old RENAME TO vtxo_ancestry_paths;

CREATE INDEX idx_vtxo_ancestry_paths_vtxo
    ON vtxo_ancestry_paths(vtxo_outpoint_hash, vtxo_outpoint_index);
