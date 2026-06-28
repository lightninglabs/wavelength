-- Remove durable wallet payment orchestration intents.

DROP INDEX IF EXISTS idx_payment_intents_state;

DROP TABLE IF EXISTS payment_intents;
