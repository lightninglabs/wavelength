-- Persist the future-channel identity and fee reserve needed to resume either
-- direct private settlement or a vHTLC-to-channel promotion after restart.
ALTER TABLE receive_swaps
    ADD COLUMN reserved_scid BLOB;

ALTER TABLE receive_swaps
    ADD COLUMN channel_backing_fee_sat BIGINT NOT NULL DEFAULT 0;

ALTER TABLE receive_swaps
    ADD COLUMN channel_id BLOB;
