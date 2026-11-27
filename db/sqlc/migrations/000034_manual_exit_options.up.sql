-- Early mobile builds used version 33 for manual exit options. Ensure both
-- tables exist before reading either without replacing saved exit terms or
-- a pending signed split.
CREATE TABLE IF NOT EXISTS wallet_fee_split (
    singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
    candidate BLOB NOT NULL
);

CREATE TABLE IF NOT EXISTS vtxo_manual_exit_options (
    outpoint_hash BLOB NOT NULL,
    outpoint_index INTEGER NOT NULL,
    sweep_script BLOB NOT NULL,
    fee_rate_sat_per_vbyte BIGINT NOT NULL CHECK (fee_rate_sat_per_vbyte >= 0),
    PRIMARY KEY (outpoint_hash, outpoint_index),
    FOREIGN KEY (outpoint_hash, outpoint_index)
        REFERENCES vtxos (outpoint_hash, outpoint_index) ON DELETE CASCADE
);
