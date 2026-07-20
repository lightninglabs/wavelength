-- VTXO state: the vtxos table plus the normalized ancestry side
-- table.
--
-- Ancestry is stored out-of-line rather than inline on the vtxos row:
-- the operator-configurable lineage cap admits up to ~100 KB of
-- cumulative on-chain bytes per VTXO, and storing that inline causes
-- read-amplification on every routine fetch (e.g. ListUnspentVTXOs
-- would haul the full ancestry blob for thousands of rows). The side
-- table lets routine queries skip ancestry entirely and only join
-- when the unroller actually needs to exit on-chain.
--
-- Shape:
--   - One vtxo_ancestry_paths row per (vtxo, ordered tree fragment).
--   - Round-direct and same-commitment OOR VTXOs hold exactly one row.
--   - Cross-commitment multi-input OOR VTXOs hold one row per distinct
--     contributing commitment tx.

-- VTXOs table.
-- Virtual Transaction Outputs owned by the client.
CREATE TABLE IF NOT EXISTS vtxos (
    -- outpoint_hash and outpoint_index form the VTXO outpoint (primary key).
    outpoint_hash BLOB NOT NULL,
    outpoint_index INTEGER NOT NULL,

    -- round_id links to the round that created this VTXO.
    round_id TEXT NOT NULL,

    -- amount is the value in satoshis.
    amount BIGINT NOT NULL,

    -- pk_script is the output script for this VTXO.
    pk_script BLOB NOT NULL,

    -- expiry is the CSV delay in blocks.
    expiry INTEGER NOT NULL,

    -- policy_template is the semantic arkscript policy for this VTXO.
    -- This is the authoritative representation; the decomposed key and delay
    -- columns remain as denormalized standard-policy helpers.
    policy_template BLOB,

    -- client_key_id references the internal_keys registry row for the local
    -- ownership (client) wallet key. The registry row carries the compressed
    -- pubkey plus the lnd KeyLocator needed to reconstruct the signing
    -- descriptor. Nullable because the round store can create a minimal VTXO
    -- row before the VTXO manager heals it with the full descriptor.
    client_key_id BIGINT REFERENCES internal_keys(id),

    -- operator_pubkey is the 33-byte compressed operator public key.
    operator_pubkey BLOB NOT NULL,

    -- batch_expiry is the absolute block height at which the batch expires
    -- (when the operator can sweep via the batch-level timelock). Zero value
    -- is used for VTXOs created via the round store before the VTXO manager
    -- fills in the full metadata via ON CONFLICT DO UPDATE.
    batch_expiry INTEGER NOT NULL,

    -- created_height is the block height when this VTXO was created.
    -- Zero for same reason.
    created_height INTEGER NOT NULL,

    -- commitment_txid is the 32-byte txid of the commitment transaction that
    -- anchors this VTXO's tree on-chain. Empty blob until the VTXO manager
    -- fills in the full metadata via ON CONFLICT DO UPDATE.
    commitment_txid BLOB NOT NULL,

    -- spent indicates if this VTXO has been used.
    spent BOOLEAN NOT NULL DEFAULT FALSE,

    -- status tracks VTXO lifecycle (vtxo.VTXOStatus enum):
    --   0 = Live (default)
    --   1 = PendingForfeit
    --   2 = Forfeiting
    --   3 = Forfeited
    --   4 = Spent
    --   5 = UnilateralExit
    --   6 = Failed
    --   7 = Spending
    --   8 = Expired
    --   9 = Redeeming
    --  10 = Redeemed
    status INTEGER NOT NULL DEFAULT 0,

    -- forfeit_round_id is the round in which this VTXO is being forfeited.
    -- NULL unless VTXO is in Forfeiting or Forfeited status.
    forfeit_round_id TEXT,

    -- forfeit_tx is the serialized wire.MsgTx (binary) of the forfeit tx.
    -- Persisted when entering Forfeiting state for crash recovery.
    forfeit_tx BLOB,

    -- forfeit_txid is the 32-byte hash of the forfeit transaction.
    -- Set when the forfeit is confirmed (transition to Forfeited state).
    forfeit_txid BLOB,

    -- replaced_by_hash is the outpoint hash of the replacement VTXO.
    replaced_by_hash BLOB,

    -- replaced_by_index is the outpoint index of the replacement VTXO.
    replaced_by_index INTEGER,

    -- creation_time is the unix epoch timestamp when this VTXO was created.
    creation_time BIGINT NOT NULL,

    -- last_update_time is the unix epoch timestamp when this VTXO was last
    -- modified, such as when it was marked as spent.
    last_update_time BIGINT NOT NULL,

    -- chain_depth tracks the number of OOR checkpoint hops between
    -- this VTXO and the most recent on-chain commitment. Round-created
    -- VTXOs have chain_depth 0.
    chain_depth INTEGER NOT NULL DEFAULT 0,

    -- construction_version records the per-VTXO construction version: the
    -- rules under which this VTXO was built and must be spent or exited. It
    -- is stamped at creation and never changes. The versions are
    -- zero-indexed, so the only understood value today is 0 (V1); a future,
    -- genuinely different construction is added additively (V2 == 1, and so
    -- on). NOT NULL DEFAULT 0 keeps every row a valid V1 object.
    construction_version INTEGER NOT NULL DEFAULT 0,

    PRIMARY KEY (outpoint_hash, outpoint_index),
    FOREIGN KEY (round_id) REFERENCES rounds(round_id)
);

-- Index on round_id for lookup by round.
CREATE INDEX IF NOT EXISTS idx_vtxos_round_id
    ON vtxos(round_id);

-- Index on spent for listing unspent VTXOs.
CREATE INDEX IF NOT EXISTS idx_vtxos_spent
    ON vtxos(spent);

-- Index on creation_time for chronological queries.
CREATE INDEX IF NOT EXISTS idx_vtxos_creation_time
    ON vtxos(creation_time DESC);

-- Index on status for efficient status-based queries (ListLiveVTXOs, etc.).
CREATE INDEX IF NOT EXISTS idx_vtxos_status
    ON vtxos(status);

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
