-- Ark channel coordination stores only facts that cross the Ark and lnd
-- databases. Lightning commitments, HTLCs, invoices, and payment attempts
-- remain exclusively in lnd.
CREATE TABLE IF NOT EXISTS ark_channels (
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

	phase INTEGER NOT NULL CHECK (phase BETWEEN 1 AND 10),
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
    CHECK (channel_delay >= 0 AND channel_delay <= 4294967295),
    CHECK (funder_delay >= 0 AND funder_delay <= 4294967295),
    CHECK (min_exit_delay >= 0 AND min_exit_delay <= 4294967295)
);

CREATE INDEX IF NOT EXISTS idx_ark_channels_phase_created
    ON ark_channels(phase, created_at ASC);

CREATE UNIQUE INDEX IF NOT EXISTS idx_ark_channels_channel_point
    ON ark_channels(channel_point_txid, channel_point_index)
    WHERE channel_point_txid IS NOT NULL;
