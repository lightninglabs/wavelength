-- Unroll store: tracks in-progress VTXO tree unrolls so that the unroller
-- can resume broadcasting from where it left off after daemon restart.
--
-- Status values: 0=pending, 1=broadcasting, 2=awaiting_csv, 3=complete,
-- 4=failed.
CREATE TABLE IF NOT EXISTS unrolls (
    vtxo_outpoint_hash BLOB NOT NULL,
    vtxo_outpoint_index INTEGER NOT NULL,
    status INTEGER NOT NULL,
    current_level INTEGER NOT NULL,
    leaf_confirm_height INTEGER NOT NULL,
    error_msg TEXT,
    retry_count INTEGER NOT NULL,
    last_broadcast_height INTEGER NOT NULL,
    current_fee_rate BIGINT NOT NULL,
    created_at BIGINT NOT NULL,
    updated_at BIGINT NOT NULL,
    PRIMARY KEY (vtxo_outpoint_hash, vtxo_outpoint_index)
);
