-- Downgrade is intentionally rejected by the destination CHECK if a
-- receive-claim channel still exists.
ALTER TABLE ark_channels RENAME TO ark_channels_v18;

CREATE TABLE ark_channels (
    channel_id BLOB PRIMARY KEY NOT NULL,
    kind INTEGER NOT NULL CHECK (kind IN (1, 2)),
    funder INTEGER NOT NULL CHECK (funder IN (1, 2)),
    pending_channel_id BLOB NOT NULL UNIQUE,
    reserved_scid BLOB NOT NULL,
    capacity BIGINT NOT NULL CHECK (capacity > 0),
    client_node_key BLOB NOT NULL,
    hub_node_key BLOB NOT NULL,
    payment_hash BLOB NOT NULL,
    client_ark_key BLOB NOT NULL,
    hub_ark_key BLOB NOT NULL,
    ark_operator_key BLOB NOT NULL,
    client_channel_key BLOB NOT NULL,
    hub_channel_key BLOB NOT NULL,
    funder_key BLOB NOT NULL,
    channel_delay BIGINT NOT NULL,
    funder_delay BIGINT NOT NULL,
    min_exit_delay BIGINT NOT NULL,

    phase INTEGER NOT NULL CHECK (phase BETWEEN 1 AND 13),
    oor_session_id BLOB,
    source_index BIGINT,
    source_amount BIGINT,
    source_ark_tx BLOB,
    backing_tx BLOB,
    channel_point_txid BLOB,
    channel_point_index BIGINT,
    client_finalized BOOLEAN NOT NULL DEFAULT FALSE,
    hub_finalized BOOLEAN NOT NULL DEFAULT FALSE,
    oor_finalized BOOLEAN NOT NULL DEFAULT FALSE,
    oor_aborted BOOLEAN NOT NULL DEFAULT FALSE,
    backing_published BOOLEAN NOT NULL DEFAULT FALSE,
    close_initiator INTEGER CHECK (close_initiator IN (1, 2)),
    close_client_script BLOB,
    close_hub_script BLOB,
    close_fee_rate_sat_per_kw BIGINT,
    cooperative_close_tx BLOB,
    cooperative_close_txid BLOB,
    close_commitment_height BIGINT,
    close_client_balance BIGINT,
    close_hub_balance BIGINT,
    client_close_signed BOOLEAN NOT NULL DEFAULT FALSE,
    hub_close_signed BOOLEAN NOT NULL DEFAULT FALSE,
    client_close_finalized BOOLEAN NOT NULL DEFAULT FALSE,
    hub_close_finalized BOOLEAN NOT NULL DEFAULT FALSE,
    failure TEXT,

    revision BIGINT NOT NULL CHECK (revision > 0),
    created_at BIGINT NOT NULL,
    updated_at BIGINT NOT NULL,

    CHECK (
        (oor_session_id IS NULL AND source_index IS NULL AND
         source_amount IS NULL AND source_ark_tx IS NULL) OR
        (oor_session_id IS NOT NULL AND source_index IS NOT NULL AND
         source_amount IS NOT NULL AND source_ark_tx IS NOT NULL)
    ),
    CHECK (
        (backing_tx IS NULL AND channel_point_txid IS NULL AND
         channel_point_index IS NULL) OR
        (backing_tx IS NOT NULL AND channel_point_txid IS NOT NULL AND
         channel_point_index IS NOT NULL)
    ),
    CHECK (
        (close_initiator IS NULL AND close_client_script IS NULL AND
         close_hub_script IS NULL AND close_fee_rate_sat_per_kw IS NULL) OR
        (close_initiator IS NOT NULL AND close_client_script IS NOT NULL AND
         close_hub_script IS NOT NULL AND close_fee_rate_sat_per_kw IS NOT NULL)
    ),
    CHECK (
        (cooperative_close_tx IS NULL AND cooperative_close_txid IS NULL AND
         close_commitment_height IS NULL AND close_client_balance IS NULL AND
         close_hub_balance IS NULL) OR
        (cooperative_close_tx IS NOT NULL AND cooperative_close_txid IS NOT NULL AND
         close_commitment_height IS NOT NULL AND close_client_balance IS NOT NULL AND
         close_hub_balance IS NOT NULL)
    ),
    CHECK (channel_delay >= 0 AND channel_delay <= 4294967295),
    CHECK (funder_delay >= 0 AND funder_delay <= 4294967295),
    CHECK (min_exit_delay >= 0 AND min_exit_delay <= 4294967295)
);

INSERT INTO ark_channels (
    channel_id, kind, funder, pending_channel_id, reserved_scid, capacity,
    client_node_key, hub_node_key, payment_hash, client_ark_key, hub_ark_key,
    ark_operator_key, client_channel_key, hub_channel_key, funder_key,
    channel_delay, funder_delay, min_exit_delay, phase, oor_session_id,
    source_index, source_amount, source_ark_tx, backing_tx,
    channel_point_txid, channel_point_index, client_finalized, hub_finalized,
    oor_finalized, oor_aborted, backing_published, close_initiator,
    close_client_script, close_hub_script, close_fee_rate_sat_per_kw,
    cooperative_close_tx, cooperative_close_txid, close_commitment_height,
    close_client_balance, close_hub_balance, client_close_signed,
    hub_close_signed, client_close_finalized, hub_close_finalized, failure,
    revision, created_at, updated_at
)
SELECT
    channel_id, kind, funder, pending_channel_id, reserved_scid, capacity,
    client_node_key, hub_node_key, payment_hash, client_ark_key, hub_ark_key,
    ark_operator_key, client_channel_key, hub_channel_key, funder_key,
    channel_delay, funder_delay, min_exit_delay, phase, oor_session_id,
    source_index, source_amount, source_ark_tx, backing_tx,
    channel_point_txid, channel_point_index, client_finalized, hub_finalized,
    oor_finalized, oor_aborted, backing_published, close_initiator,
    close_client_script, close_hub_script, close_fee_rate_sat_per_kw,
    cooperative_close_tx, cooperative_close_txid, close_commitment_height,
    close_client_balance, close_hub_balance, client_close_signed,
    hub_close_signed, client_close_finalized, hub_close_finalized, failure,
    revision, created_at, updated_at
FROM ark_channels_v18;

DROP TABLE ark_channels_v18;

CREATE INDEX idx_ark_channels_phase_created
    ON ark_channels(phase, created_at ASC);

CREATE UNIQUE INDEX idx_ark_channels_channel_point
    ON ark_channels(channel_point_txid, channel_point_index)
    WHERE channel_point_txid IS NOT NULL;
