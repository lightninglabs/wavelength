-- Remove durable settlement-rail metadata.

ALTER TABLE receive_swaps
    DROP COLUMN settlement_type;

ALTER TABLE pay_swaps
    DROP COLUMN settlement_type;
