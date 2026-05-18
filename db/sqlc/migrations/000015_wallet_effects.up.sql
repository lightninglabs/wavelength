CREATE TABLE IF NOT EXISTS wallet_effects (
    id TEXT PRIMARY KEY,
    effect_type TEXT NOT NULL CHECK (effect_type IN (
        'record_ledger_sweep_fee',
        'record_ledger_utxo_created',
        'record_ledger_utxo_spent'
    )),
    status TEXT NOT NULL CHECK (status IN (
        'pending', 'claimed', 'done', 'dead'
    )),
    idempotency_key TEXT NOT NULL UNIQUE,

    outpoint_hash BLOB,
    outpoint_index INTEGER,
    txid BLOB,
    amount_sat BIGINT,
    fee_sat BIGINT,
    block_height INTEGER,
    classification TEXT,

    attempts INTEGER NOT NULL DEFAULT 0,
    max_attempts INTEGER NOT NULL DEFAULT 10,
    next_attempt_at BIGINT NOT NULL,
    claim_owner TEXT,
    claim_token TEXT,
    claim_until BIGINT,
    last_error TEXT,

    created_at BIGINT NOT NULL,
    updated_at BIGINT NOT NULL,
    done_at BIGINT,

    CHECK (id <> ''),
    CHECK (idempotency_key <> ''),
    CHECK (attempts >= 0),
    CHECK (max_attempts > 0),
    CHECK (next_attempt_at > 0),
    CHECK (created_at > 0),
    CHECK (updated_at >= created_at)
);

CREATE INDEX IF NOT EXISTS idx_wallet_effects_due
    ON wallet_effects(status, next_attempt_at, created_at);

