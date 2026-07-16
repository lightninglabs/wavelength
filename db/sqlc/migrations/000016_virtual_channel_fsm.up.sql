INSERT INTO virtual_channel_statuses (status) VALUES
	('requested'),
	('round_requested'),
	('funding_bound'),
	('lnd_negotiating'),
	('funding_verified'),
	('backing_armed'),
	('round_confirmed'),
	('funding_published')
ON CONFLICT DO NOTHING;

ALTER TABLE virtual_channels
	ADD COLUMN kind TEXT NOT NULL DEFAULT 'promote_vtxo';
ALTER TABLE virtual_channels ADD COLUMN round_id TEXT;
ALTER TABLE virtual_channels
	ADD COLUMN state_version BIGINT NOT NULL DEFAULT 1
		CHECK (state_version > 0);
ALTER TABLE virtual_channels ADD COLUMN backing_armed_at BIGINT;

UPDATE virtual_channels
SET backing_armed_at = updated_at
WHERE status IN ('active', 'materializing', 'closing', 'closed');

ALTER TABLE virtual_channel_intents
	ADD COLUMN kind TEXT NOT NULL DEFAULT 'promote_vtxo';
ALTER TABLE virtual_channel_intents ADD COLUMN round_id TEXT;
ALTER TABLE virtual_channel_intents ADD COLUMN request_key TEXT;
ALTER TABLE virtual_channel_intents
	ADD COLUMN state_version BIGINT NOT NULL DEFAULT 1
		CHECK (state_version > 0);

UPDATE virtual_channels
SET status = 'lnd_negotiating'
WHERE status = 'negotiating';

UPDATE virtual_channel_intents
SET status = 'funding_bound'
WHERE status = 'negotiating';

CREATE INDEX IF NOT EXISTS idx_virtual_channels_round
	ON virtual_channels(kind, round_id, status);
CREATE UNIQUE INDEX IF NOT EXISTS idx_virtual_channel_intents_request
	ON virtual_channel_intents(kind, role, request_key)
	WHERE request_key IS NOT NULL;

DROP INDEX IF EXISTS idx_virtual_channel_vtxos_outpoint;
DROP INDEX IF EXISTS idx_virtual_channel_intent_vtxos_outpoint;
CREATE UNIQUE INDEX idx_virtual_channel_vtxos_outpoint
	ON virtual_channel_vtxos(outpoint_hash, outpoint_index);
CREATE UNIQUE INDEX idx_virtual_channel_intent_vtxos_outpoint
	ON virtual_channel_intent_vtxos(outpoint_hash, outpoint_index);
