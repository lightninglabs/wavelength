-- wavelength#969: an OOR Ark tx may spend two input VTXOs that both descend
-- from the same commitment tx but sit at different leaves -- e.g. the change
-- VTXO of a send that consumed two coins from one round. The produced VTXO
-- then legitimately carries two ancestry fragments with the same
-- commitment_txid but distinct leaves (each needs its own root->leaf path) and
-- distinct Ark input indices.
--
-- The original UNIQUE(vtxo_outpoint_hash, vtxo_outpoint_index, commitment_txid)
-- forbade that: persisting such a VTXO failed, and the receive path rejected
-- the change before it could be credited, dropping the sender's balance to
-- zero. Row identity is already provided by the PRIMARY KEY (..., path_order),
-- and redundant/malformed fragments are now rejected in Go on the
-- (commitment, input index) pair (upsertAncestryPaths / validateIncomingAncestry),
-- so this schema-level uniqueness is both wrong and unnecessary.
--
-- SQLite cannot drop an inline constraint, so the table is rebuilt without the
-- UNIQUE. Nothing references vtxo_ancestry_paths as a foreign-key parent, so
-- the rebuild is safe with foreign_keys=ON (the table is a pure child of
-- vtxos). commitment_height (added in 000013) is folded in inline here.
CREATE TABLE vtxo_ancestry_paths_new (
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

    -- path_order must be a small non-negative ordinal (see 000004).
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

CREATE INDEX idx_vtxo_ancestry_paths_vtxo
    ON vtxo_ancestry_paths(vtxo_outpoint_hash, vtxo_outpoint_index);
