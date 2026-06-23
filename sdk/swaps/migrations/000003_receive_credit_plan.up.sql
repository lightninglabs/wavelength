-- Add durable receive credit-assist planning metadata.

ALTER TABLE receive_swaps
    ADD COLUMN requested_amount_sat BIGINT NOT NULL DEFAULT 0;

ALTER TABLE receive_swaps
    ADD COLUMN available_credit_sat BIGINT NOT NULL DEFAULT 0;

ALTER TABLE receive_swaps
    ADD COLUMN attached_credit_sat BIGINT NOT NULL DEFAULT 0;

ALTER TABLE receive_swaps
    ADD COLUMN dust_limit_sat BIGINT NOT NULL DEFAULT 0;
