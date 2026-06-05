DROP INDEX IF EXISTS idx_virtual_channel_vtxos_outpoint;

CREATE TABLE virtual_channel_intent_vtxos_old (
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

INSERT INTO virtual_channel_intent_vtxos_old (
	pending_channel_id, outpoint_hash, outpoint_index, amount_sat
)
SELECT pending_channel_id, outpoint_hash, outpoint_index, amount_sat
FROM virtual_channel_intent_vtxos;

DROP TABLE virtual_channel_intent_vtxos;
ALTER TABLE virtual_channel_intent_vtxos_old
	RENAME TO virtual_channel_intent_vtxos;

CREATE TABLE virtual_channel_vtxos_old (
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

INSERT INTO virtual_channel_vtxos_old (
	virtual_channel_id, outpoint_hash, outpoint_index, amount_sat
)
SELECT virtual_channel_id, outpoint_hash, outpoint_index, amount_sat
FROM virtual_channel_vtxos;

DROP TABLE virtual_channel_vtxos;
ALTER TABLE virtual_channel_vtxos_old
	RENAME TO virtual_channel_vtxos;

CREATE INDEX IF NOT EXISTS idx_virtual_channel_vtxos_outpoint
	ON virtual_channel_vtxos(outpoint_hash, outpoint_index);
