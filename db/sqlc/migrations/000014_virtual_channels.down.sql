-- Revert 000017_virtual_channels.

DROP INDEX IF EXISTS idx_virtual_channel_vtxos_outpoint;
DROP INDEX IF EXISTS idx_virtual_channel_intent_vtxos_outpoint;
DROP INDEX IF EXISTS idx_virtual_channels_channel_point;
DROP INDEX IF EXISTS idx_virtual_channels_status;
DROP TABLE IF EXISTS virtual_channel_vtxos;
DROP TABLE IF EXISTS virtual_channel_intent_vtxos;
DROP TABLE IF EXISTS virtual_channel_intents;
DROP TABLE IF EXISTS virtual_channels;
DROP TABLE IF EXISTS virtual_channel_roles;
DROP TABLE IF EXISTS virtual_channel_statuses;
