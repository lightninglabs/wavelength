ALTER TABLE pay_swaps
    ADD COLUMN server_fee_sat BIGINT NOT NULL DEFAULT 0;

ALTER TABLE pay_swaps
    ADD COLUMN routing_fee_budget_sat BIGINT NOT NULL DEFAULT 0;
