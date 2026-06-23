-- Remove durable receive credit-assist planning metadata.

ALTER TABLE receive_swaps
    DROP COLUMN dust_limit_sat;

ALTER TABLE receive_swaps
    DROP COLUMN attached_credit_sat;

ALTER TABLE receive_swaps
    DROP COLUMN available_credit_sat;

ALTER TABLE receive_swaps
    DROP COLUMN requested_amount_sat;
