-- Persist the future-channel identity and ownership needed to resume direct
-- private settlement after restart.
ALTER TABLE receive_swaps
    ADD COLUMN reserved_scid BLOB;

ALTER TABLE receive_swaps
    ADD COLUMN channel_receive_enabled BOOLEAN NOT NULL DEFAULT FALSE;

ALTER TABLE receive_swaps
    ADD COLUMN channel_id BLOB;
