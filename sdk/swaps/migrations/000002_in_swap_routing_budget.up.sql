ALTER TABLE pay_swaps
    -- server_fee_sat is the service-fee portion of the accepted total fee.
    ADD COLUMN server_fee_sat BIGINT NOT NULL DEFAULT 0;

ALTER TABLE pay_swaps
    -- routing_fee_budget_sat is the client-funded Lightning fee allowance.
    ADD COLUMN routing_fee_budget_sat BIGINT NOT NULL DEFAULT 0;
