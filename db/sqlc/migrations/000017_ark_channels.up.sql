-- Ark channel coordination stores only facts that cross the Ark and lnd
-- databases. Lightning commitments, HTLCs, invoices, and payment attempts
-- remain exclusively in lnd.
CREATE TABLE IF NOT EXISTS ark_channels (
    channel_id BLOB PRIMARY KEY NOT NULL,
    kind INTEGER NOT NULL CHECK (kind IN (1, 2)),
    funder INTEGER NOT NULL CHECK (funder IN (1, 2)),
    pending_channel_id BLOB NOT NULL,
    reserved_scid BLOB NOT NULL,
    capacity BIGINT NOT NULL CHECK (capacity > 0),
    client_node_key BLOB NOT NULL,
    hub_node_key BLOB NOT NULL,
    payment_hash BLOB NOT NULL,
    policy_template BLOB NOT NULL,
    pk_script BLOB NOT NULL,

    phase INTEGER NOT NULL CHECK (phase BETWEEN 1 AND 10),
    source_txid BLOB,
    source_index BIGINT,
    source_amount BIGINT,
    round_id TEXT,
    commitment_txid BLOB,
    backing_tx BLOB,
    channel_point_txid BLOB,
    channel_point_index BIGINT,
    client_finalized BOOLEAN NOT NULL DEFAULT FALSE,
    hub_finalized BOOLEAN NOT NULL DEFAULT FALSE,
    round_committed BOOLEAN NOT NULL DEFAULT FALSE,
    round_confirmed BOOLEAN NOT NULL DEFAULT FALSE,
    backing_published BOOLEAN NOT NULL DEFAULT FALSE,
    failure TEXT,

    revision BIGINT NOT NULL CHECK (revision > 0),
    created_at BIGINT NOT NULL,
    updated_at BIGINT NOT NULL,

    CHECK (
        (source_txid IS NULL AND source_index IS NULL AND
         source_amount IS NULL AND round_id IS NULL AND
         commitment_txid IS NULL) OR
        (source_txid IS NOT NULL AND source_index IS NOT NULL AND
         source_amount IS NOT NULL AND round_id IS NOT NULL AND
         commitment_txid IS NOT NULL)
    ),
    CHECK (
        (backing_tx IS NULL AND channel_point_txid IS NULL AND
         channel_point_index IS NULL) OR
        (backing_tx IS NOT NULL AND channel_point_txid IS NOT NULL AND
         channel_point_index IS NOT NULL)
    )
);

CREATE INDEX IF NOT EXISTS idx_ark_channels_phase_created
    ON ark_channels(phase, created_at ASC);
