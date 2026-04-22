-- UTXO audit log for tracking wallet UTXO set changes per
-- block for tax and accounting purposes.

-- Classification enum for wallet UTXO events.
CREATE TABLE IF NOT EXISTS utxo_classifications (
    classification TEXT PRIMARY KEY
);

INSERT INTO utxo_classifications (classification) VALUES
    ('deposit'),
    ('sweep_return'),
    ('round_funding'),
    ('change'),
    ('unknown')
ON CONFLICT DO NOTHING;

-- UTXO event enum (created or spent).
CREATE TABLE IF NOT EXISTS utxo_events (
    event TEXT PRIMARY KEY
);

INSERT INTO utxo_events (event) VALUES
    ('created'),
    ('spent')
ON CONFLICT DO NOTHING;

-- Wallet UTXO audit log. Each row records a single UTXO being
-- created or spent, classified by its likely cause.
CREATE TABLE IF NOT EXISTS wallet_utxo_log (
    entry_id INTEGER PRIMARY KEY,

    -- outpoint_hash is the transaction hash (32 bytes).
    outpoint_hash BLOB NOT NULL,

    -- outpoint_index is the output index.
    outpoint_index INTEGER NOT NULL,

    -- amount_sat is the UTXO value.
    amount_sat BIGINT NOT NULL,

    -- event is 'created' or 'spent'.
    event TEXT NOT NULL
        REFERENCES utxo_events(event),

    -- block_height is the block where this change occurred.
    block_height INTEGER NOT NULL,

    -- classified_as categorizes the UTXO event.
    classified_as TEXT NOT NULL
        REFERENCES utxo_classifications(classification),

    -- created_at is the Unix timestamp when this entry was
    -- recorded.
    created_at BIGINT NOT NULL,

    -- (outpoint, event) is unique across the log. The diff loop
    -- that writes these rows runs every block and may retry after
    -- a crash, so the audit sink uses ON CONFLICT DO NOTHING to
    -- make replay a silent no-op. Note that (hash, index) alone
    -- is not unique: a single outpoint can appear once with
    -- event='created' and again with event='spent' over its
    -- lifetime.
    UNIQUE (outpoint_hash, outpoint_index, event)
);

CREATE INDEX IF NOT EXISTS idx_utxo_log_block
    ON wallet_utxo_log(block_height);

CREATE INDEX IF NOT EXISTS idx_utxo_log_outpoint
    ON wallet_utxo_log(outpoint_hash, outpoint_index);

CREATE INDEX IF NOT EXISTS idx_utxo_log_classification
    ON wallet_utxo_log(classified_as);

-- Note: UTXO-set state changes (created/spent) live here and
-- are classified by `classified_as` (deposit, sweep_return,
-- round_funding, change, unknown). The ledger_entries table
-- in migration 000010 carries only economic events (money
-- movement between accounts) and does not duplicate UTXO
-- state. A UTXO diff that finds a deposit writes both a row
-- here (classified_as='deposit') and a ledger entry
-- (event_type='external_deposit') — the two tables serve
-- distinct purposes.
