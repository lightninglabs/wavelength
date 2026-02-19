-- Chain resolver state persistence supports crash recovery for active VTXO
-- unroll operations. Each row tracks a per-VTXO resolver FSM's current state
-- so the coordinator can reconstruct in-progress resolvers on restart.

CREATE TABLE IF NOT EXISTS chain_resolver_states (
    -- outpoint_hash is the VTXO outpoint txid bytes.
    outpoint_hash BLOB NOT NULL,

    -- outpoint_index is the VTXO outpoint output index.
    outpoint_index INTEGER NOT NULL CHECK (outpoint_index >= 0),

    -- state is the FSM state name (broadcasting_tree,
    -- broadcasting_checkpoints, watching_commitment, resolved, failed).
    state TEXT NOT NULL,

    -- state_details is the JSON-serialized state-specific fields. NULL for
    -- states that carry no additional data (watching_commitment, resolved,
    -- failed).
    state_details BLOB,

    -- created_at is the unix timestamp when the resolver was first persisted.
    created_at BIGINT NOT NULL,

    -- updated_at is the unix timestamp of the last state update.
    updated_at BIGINT NOT NULL,

    -- Primary key enforces one resolver row per VTXO outpoint.
    PRIMARY KEY (outpoint_hash, outpoint_index)
);

-- Index speeds listing active (non-terminal) resolvers on startup.
CREATE INDEX IF NOT EXISTS idx_chain_resolver_states_state
    ON chain_resolver_states(state);
