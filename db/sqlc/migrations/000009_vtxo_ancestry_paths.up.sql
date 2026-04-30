-- Replace the per-VTXO singular tree_path / tree_depth columns with a
-- normalized side table that holds one row per ancestry tree fragment.
--
-- Motivation: the operator-configurable lineage cap admits up to ~100 KB
-- of cumulative on-chain bytes per VTXO. Storing that inline on the
-- vtxos row causes read-amplification on every routine fetch (e.g.
-- `ListUnspentVTXOs` would haul the full ancestry blob for thousands of
-- rows). A side table lets routine queries skip ancestry entirely and
-- only join when the unroller actually needs to exit on-chain.
--
-- Schema:
--   - One vtxo_ancestry_paths row per (vtxo, ordered tree fragment).
--   - Round-direct and same-commitment OOR VTXOs hold exactly one row.
--   - Cross-commitment multi-input OOR VTXOs hold one row per distinct
--     contributing commitment tx.
--
-- Backward compatibility is intentionally not preserved; the old
-- columns are dropped rather than carried alongside the new shape.

ALTER TABLE vtxos DROP COLUMN tree_path;
ALTER TABLE vtxos DROP COLUMN tree_depth;

-- vtxo_ancestry_paths is a one-to-many side table keyed by VTXO outpoint.
-- ON DELETE CASCADE keeps the side table consistent if the VTXO row is
-- ever deleted.
CREATE TABLE vtxo_ancestry_paths (
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
    -- fragment. Distinct rows for one VTXO must have distinct
    -- commitment_txids.
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

    PRIMARY KEY (vtxo_outpoint_hash, vtxo_outpoint_index, path_order),
    FOREIGN KEY (vtxo_outpoint_hash, vtxo_outpoint_index)
        REFERENCES vtxos(outpoint_hash, outpoint_index)
        ON DELETE CASCADE,

    -- A VTXO must not carry two ancestry rows for the same commitment
    -- tx. Distinct fragments must anchor at distinct commitments
    -- (per the Ancestry contract); enforcing it at the schema level
    -- means a future caller bypassing BuildIncomingVTXODescriptor
    -- still cannot persist a malformed VTXO that would later trip a
    -- "conflicting proof node" deep inside addProofNode at unilateral
    -- exit time.
    UNIQUE (vtxo_outpoint_hash, vtxo_outpoint_index, commitment_txid),

    -- path_order must be a small non-negative ordinal. The active
    -- fragment-count cap (MaxAncestryFragments) is well under 64;
    -- this CHECK guards against a caller persisting a row at a
    -- nonsense ordinal (e.g. negative, or a uint32 round-trip from
    -- malformed wire data) without coupling the schema to the exact
    -- runtime cap.
    CHECK (path_order >= 0 AND path_order < 64)
);

-- Lookup index for the common path-by-vtxo query in the unroller.
CREATE INDEX idx_vtxo_ancestry_paths_vtxo
    ON vtxo_ancestry_paths(vtxo_outpoint_hash, vtxo_outpoint_index);
