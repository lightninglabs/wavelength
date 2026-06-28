-- Add durable wallet payment orchestration intents.

CREATE TABLE IF NOT EXISTS payment_intents (
    payment_hash BLOB PRIMARY KEY,
    invoice TEXT NOT NULL,
    max_fee_sat BIGINT NOT NULL,
    max_credit_sat BIGINT NOT NULL,
    max_credit_topup_sat BIGINT NOT NULL,
    state TEXT NOT NULL,
    credit_idempotency_key TEXT NOT NULL,
    credit_operation_id TEXT NOT NULL DEFAULT '',
    credit_topup_sat BIGINT NOT NULL DEFAULT 0,
    credit_destination_pubkey BLOB,
    credit_oor_session_id TEXT NOT NULL DEFAULT '',
    pay_started_hash BLOB,
    last_error TEXT NOT NULL DEFAULT '',
    created_at_unix BIGINT NOT NULL,
    updated_at_unix BIGINT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_payment_intents_state
    ON payment_intents(state, updated_at_unix);
