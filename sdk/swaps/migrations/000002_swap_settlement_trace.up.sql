-- Add durable settlement-rail metadata for swap inspection.

ALTER TABLE receive_swaps
    ADD COLUMN settlement_type TEXT NOT NULL DEFAULT '';

ALTER TABLE pay_swaps
    ADD COLUMN settlement_type TEXT NOT NULL DEFAULT '';
