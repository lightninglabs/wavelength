ALTER TABLE credit_operations
    ADD COLUMN routing_fee_budget_sat BIGINT NOT NULL DEFAULT 0;
