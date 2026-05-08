-- Boarding sweep tracking.
--
-- A broadcast boarding sweep is not complete until the chain backend reports
-- the swept boarding outpoints as spent. These tables persist the published
-- sweep transaction and each input so the daemon can resume spend watches and
-- rebroadcast the exact same transaction after restart.

INSERT INTO boarding_statuses (id, status_name) VALUES
    (5, 'sweep_pending');

CREATE TABLE IF NOT EXISTS boarding_sweeps (
    txid BLOB PRIMARY KEY NOT NULL,
    raw_tx BLOB NOT NULL,
    destination_address TEXT NOT NULL,
    total_amount BIGINT NOT NULL,
    fee_amount BIGINT NOT NULL,
    fee_rate_sat_per_vbyte BIGINT NOT NULL,
    vbytes BIGINT NOT NULL,
    status TEXT NOT NULL CHECK (
        status IN (
            'pending',
            'published',
            'confirmed',
            'external_resolved',
            'failed'
        )
    ),
    created_height INTEGER NOT NULL,
    created_time BIGINT NOT NULL,
    published_time BIGINT,
    confirmed_height INTEGER,
    last_error TEXT
);

CREATE INDEX IF NOT EXISTS idx_boarding_sweeps_status
    ON boarding_sweeps(status);

CREATE TABLE IF NOT EXISTS boarding_sweep_inputs (
    txid BLOB NOT NULL,
    outpoint_hash BLOB NOT NULL,
    outpoint_index INTEGER NOT NULL,
    amount BIGINT NOT NULL,
    previous_status TEXT NOT NULL,
    status TEXT NOT NULL CHECK (
        status IN (
            'pending',
            'published',
            'spent',
            'external_spent',
            'failed'
        )
    ),
    spent_by_txid BLOB,
    spent_height INTEGER,
    last_update_time BIGINT NOT NULL,

    PRIMARY KEY (txid, outpoint_hash, outpoint_index),
    FOREIGN KEY (txid) REFERENCES boarding_sweeps(txid),
    FOREIGN KEY (previous_status) REFERENCES boarding_statuses(status_name),
    FOREIGN KEY (outpoint_hash, outpoint_index)
        REFERENCES boarding_intents(outpoint_hash, outpoint_index)
);

CREATE INDEX IF NOT EXISTS idx_boarding_sweep_inputs_status
    ON boarding_sweep_inputs(status);

CREATE INDEX IF NOT EXISTS idx_boarding_sweep_inputs_outpoint
    ON boarding_sweep_inputs(outpoint_hash, outpoint_index);

CREATE UNIQUE INDEX IF NOT EXISTS idx_boarding_sweep_inputs_active_outpoint
    ON boarding_sweep_inputs(outpoint_hash, outpoint_index)
    WHERE status IN ('pending', 'published');
