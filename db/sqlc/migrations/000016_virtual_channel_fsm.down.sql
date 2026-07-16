DROP INDEX IF EXISTS idx_virtual_channels_round;
DROP INDEX IF EXISTS idx_virtual_channel_intents_request;
DROP INDEX IF EXISTS idx_virtual_channel_vtxos_outpoint;
DROP INDEX IF EXISTS idx_virtual_channel_intent_vtxos_outpoint;
CREATE INDEX idx_virtual_channel_vtxos_outpoint
	ON virtual_channel_vtxos(outpoint_hash, outpoint_index);
CREATE INDEX idx_virtual_channel_intent_vtxos_outpoint
	ON virtual_channel_intent_vtxos(outpoint_hash, outpoint_index);

UPDATE virtual_channels
SET status = 'active'
WHERE status = 'funding_published';

UPDATE virtual_channels
SET status = 'negotiating'
WHERE status IN (
	'funding_bound', 'lnd_negotiating', 'funding_verified', 'backing_armed',
	'round_confirmed'
);

UPDATE virtual_channel_intents
SET status = 'negotiating'
WHERE status IN (
	'requested', 'round_requested', 'funding_bound', 'lnd_negotiating'
);

ALTER TABLE virtual_channel_intents DROP COLUMN state_version;
ALTER TABLE virtual_channel_intents DROP COLUMN request_key;
ALTER TABLE virtual_channel_intents DROP COLUMN round_id;
ALTER TABLE virtual_channel_intents DROP COLUMN kind;
ALTER TABLE virtual_channels DROP COLUMN backing_armed_at;
ALTER TABLE virtual_channels DROP COLUMN state_version;
ALTER TABLE virtual_channels DROP COLUMN round_id;
ALTER TABLE virtual_channels DROP COLUMN kind;

DELETE FROM virtual_channel_statuses
WHERE status IN (
	'requested', 'funding_bound', 'lnd_negotiating', 'funding_verified',
	'backing_armed',
	'round_requested', 'round_confirmed', 'funding_published'
);
