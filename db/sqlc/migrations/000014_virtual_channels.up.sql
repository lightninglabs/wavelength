-- Virtual Lightning channel registration.
--
-- A virtual channel is an lnd channel whose funding outpoint is the output of
-- a fully signed VTXO spend that has not been broadcast in the happy path.
-- Conflict publication uses these rows to publish the parent before lnd
-- publishes any dependent commitment or HTLC transaction.

CREATE TABLE IF NOT EXISTS virtual_channel_statuses (
	status TEXT PRIMARY KEY
);

INSERT INTO virtual_channel_statuses (status) VALUES
	('negotiating'),
	('active'),
	('materializing'),
	('closing'),
	('closed'),
	('failed')
ON CONFLICT DO NOTHING;

CREATE TABLE IF NOT EXISTS virtual_channel_roles (
	role TEXT PRIMARY KEY
);

INSERT INTO virtual_channel_roles (role) VALUES
	('client'),
	('operator')
ON CONFLICT DO NOTHING;

CREATE TABLE IF NOT EXISTS virtual_channels (
	virtual_channel_id BLOB NOT NULL PRIMARY KEY,
	pending_channel_id BLOB NOT NULL UNIQUE,
	channel_point_hash BLOB NOT NULL,
	channel_point_index INTEGER NOT NULL,
	remote_node_pubkey BLOB NOT NULL,
	role TEXT NOT NULL,
	status TEXT NOT NULL,
	capacity_sat BIGINT NOT NULL CHECK (capacity_sat > 0),
	local_balance_sat BIGINT NOT NULL CHECK (local_balance_sat >= 0),
	remote_balance_sat BIGINT NOT NULL CHECK (remote_balance_sat >= 0),
	backing_tx BLOB NOT NULL,
	funding_psbt BLOB NOT NULL,
	close_tx BLOB,
	created_at BIGINT NOT NULL,
	updated_at BIGINT NOT NULL,
	materialized_at BIGINT,
	closed_at BIGINT,

	UNIQUE(channel_point_hash, channel_point_index),
	FOREIGN KEY (role) REFERENCES virtual_channel_roles(role),
	FOREIGN KEY (status) REFERENCES virtual_channel_statuses(status),
	CHECK (length(virtual_channel_id) = 32),
	CHECK (length(pending_channel_id) = 32),
	CHECK (length(channel_point_hash) = 32),
	CHECK (length(remote_node_pubkey) = 33),
	CHECK (local_balance_sat + remote_balance_sat <= capacity_sat)
);

CREATE TABLE IF NOT EXISTS virtual_channel_intents (
	pending_channel_id BLOB NOT NULL PRIMARY KEY,
	remote_node_pubkey BLOB NOT NULL,
	role TEXT NOT NULL,
	status TEXT NOT NULL,
	capacity_sat BIGINT NOT NULL CHECK (capacity_sat > 0),
	local_balance_sat BIGINT NOT NULL CHECK (local_balance_sat >= 0),
	remote_balance_sat BIGINT NOT NULL CHECK (remote_balance_sat >= 0),
	created_at BIGINT NOT NULL,
	updated_at BIGINT NOT NULL,

	FOREIGN KEY (role) REFERENCES virtual_channel_roles(role),
	FOREIGN KEY (status) REFERENCES virtual_channel_statuses(status),
	CHECK (length(pending_channel_id) = 32),
	CHECK (length(remote_node_pubkey) = 33),
	CHECK (local_balance_sat + remote_balance_sat <= capacity_sat)
);

CREATE TABLE IF NOT EXISTS virtual_channel_intent_vtxos (
	pending_channel_id BLOB NOT NULL,
	outpoint_hash BLOB NOT NULL,
	outpoint_index INTEGER NOT NULL,
	amount_sat BIGINT NOT NULL CHECK (amount_sat > 0),

	PRIMARY KEY (
		pending_channel_id, outpoint_hash, outpoint_index
	),
	FOREIGN KEY (pending_channel_id)
		REFERENCES virtual_channel_intents(pending_channel_id)
		ON DELETE CASCADE,
	FOREIGN KEY (outpoint_hash, outpoint_index)
		REFERENCES vtxos(outpoint_hash, outpoint_index),
	CHECK (length(pending_channel_id) = 32),
	CHECK (length(outpoint_hash) = 32)
);

CREATE TABLE IF NOT EXISTS virtual_channel_vtxos (
	virtual_channel_id BLOB NOT NULL,
	outpoint_hash BLOB NOT NULL,
	outpoint_index INTEGER NOT NULL,
	amount_sat BIGINT NOT NULL CHECK (amount_sat > 0),

	PRIMARY KEY (
		virtual_channel_id, outpoint_hash, outpoint_index
	),
	FOREIGN KEY (virtual_channel_id)
		REFERENCES virtual_channels(virtual_channel_id)
		ON DELETE CASCADE,
	FOREIGN KEY (outpoint_hash, outpoint_index)
		REFERENCES vtxos(outpoint_hash, outpoint_index),
	CHECK (length(virtual_channel_id) = 32),
	CHECK (length(outpoint_hash) = 32)
);

CREATE INDEX IF NOT EXISTS idx_virtual_channels_status
	ON virtual_channels(status, updated_at);

CREATE INDEX IF NOT EXISTS idx_virtual_channels_channel_point
	ON virtual_channels(channel_point_hash, channel_point_index);

CREATE INDEX IF NOT EXISTS idx_virtual_channel_vtxos_outpoint
	ON virtual_channel_vtxos(outpoint_hash, outpoint_index);

CREATE INDEX IF NOT EXISTS idx_virtual_channel_intent_vtxos_outpoint
	ON virtual_channel_intent_vtxos(outpoint_hash, outpoint_index);
