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
    created_at BIGINT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_utxo_log_block
    ON wallet_utxo_log(block_height);

CREATE INDEX IF NOT EXISTS idx_utxo_log_outpoint
    ON wallet_utxo_log(outpoint_hash, outpoint_index);

CREATE INDEX IF NOT EXISTS idx_utxo_log_classification
    ON wallet_utxo_log(classified_as);

-- An outpoint can appear twice total (once 'created' and once
-- 'spent') but never twice with the same (outpoint, event). The
-- durable ledger actor replays unprocessed messages on startup via
-- RestartMessage, so without this constraint a crash between the
-- insert and the mailbox ack would produce duplicate rows.
CREATE UNIQUE INDEX IF NOT EXISTS idx_utxo_log_outpoint_event
    ON wallet_utxo_log(outpoint_hash, outpoint_index, event);
