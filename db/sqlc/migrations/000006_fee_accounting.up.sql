-- Client-side fee accounting tables using double-entry
-- bookkeeping, mirroring the server-side schema.

-- Account type enum table.
CREATE TABLE IF NOT EXISTS account_types (
    account_type TEXT PRIMARY KEY
);

INSERT INTO account_types (account_type) VALUES
    ('asset'),
    ('liability'),
    ('revenue'),
    ('expense')
ON CONFLICT DO NOTHING;

-- Ledger event type enum table.
CREATE TABLE IF NOT EXISTS ledger_event_types (
    event_type TEXT PRIMARY KEY
);

-- Client-side event types track fees paid by the user.
INSERT INTO ledger_event_types (event_type) VALUES
    ('boarding_fee_paid'),
    ('refresh_fee_paid'),
    ('onchain_fee_paid'),
    ('vtxo_received'),
    ('vtxo_sent')
ON CONFLICT DO NOTHING;

-- Chart of accounts from the client's perspective. transfers_in
-- (revenue) and transfers_out (expense) are kept as separate
-- accounts so gross send and gross receive flows are visible
-- independently instead of netted on a single account. This
-- matters for tax reporting where gross figures are typically
-- required.
CREATE TABLE IF NOT EXISTS accounts (
    account_id TEXT PRIMARY KEY,
    account_name TEXT NOT NULL,
    account_type TEXT NOT NULL
        REFERENCES account_types(account_type)
);

INSERT INTO accounts (account_id, account_name, account_type) VALUES
    ('wallet_balance', 'Wallet Balance', 'asset'),
    ('vtxo_balance',   'VTXO Balance',   'asset'),
    ('fees_paid',      'Fees Paid',          'expense'),  -- Ark protocol fees to operator
    ('onchain_fees',   'On-Chain Fees Paid', 'expense'),  -- L1 chain/miner fees
    ('transfers_in',   'Transfers In',       'revenue'),  -- counterparty side of received VTXOs
    ('transfers_out',  'Transfers Out',      'expense')   -- counterparty side of sent VTXOs
ON CONFLICT DO NOTHING;

-- Double-entry ledger for client fee tracking.
CREATE TABLE IF NOT EXISTS ledger_entries (
    entry_id INTEGER PRIMARY KEY,

    debit_account TEXT NOT NULL
        REFERENCES accounts(account_id),

    credit_account TEXT NOT NULL
        REFERENCES accounts(account_id),

    -- amount_sat is the entry amount in satoshis.
    amount_sat BIGINT NOT NULL CHECK (amount_sat > 0),

    -- round_id optionally links this entry to a round
    -- (16-byte UUID).
    round_id BLOB,

    -- session_id optionally links this entry to an OOR session
    -- (32-byte identifier). Kept as a distinct column from
    -- round_id so 16-byte rounds and 32-byte sessions do not
    -- share a type-overloaded column.
    session_id BLOB,

    -- idempotency_key is an optional outpoint-derived dedup
    -- key used by events that carry neither a round_id nor an
    -- OOR session_id (e.g. unilateral exit legs keyed by the
    -- exited VTXO's outpoint). Together with the partial unique
    -- index idx_client_ledger_idempotent_key below, it makes
    -- replay-after-crash a silent no-op for multi-leg events
    -- that would otherwise double-book on at-least-once
    -- delivery.
    idempotency_key BLOB,

    -- event_type classifies the entry.
    event_type TEXT NOT NULL
        REFERENCES ledger_event_types(event_type),

    -- description is a human-readable note.
    description TEXT NOT NULL,

    -- created_at is the Unix timestamp.
    created_at BIGINT NOT NULL,

    -- Debit and credit must target different accounts.
    CHECK (debit_account != credit_account)
);

CREATE INDEX IF NOT EXISTS idx_client_ledger_created
    ON ledger_entries(created_at DESC);

CREATE INDEX IF NOT EXISTS idx_client_ledger_event_type
    ON ledger_entries(event_type);

CREATE INDEX IF NOT EXISTS idx_client_ledger_round
    ON ledger_entries(round_id);

CREATE INDEX IF NOT EXISTS idx_client_ledger_debit
    ON ledger_entries(debit_account);

CREATE INDEX IF NOT EXISTS idx_client_ledger_credit
    ON ledger_entries(credit_account);

-- Prevent duplicate entries for the same round, event, and
-- account pair. Entries with NULL round_id flow through the
-- session_id or idempotency_key partial indexes instead so
-- every event class gets its own at-least-once-idempotent path
-- without colliding.
CREATE UNIQUE INDEX IF NOT EXISTS idx_client_ledger_idempotent_round
    ON ledger_entries(round_id, event_type, debit_account, credit_account)
    WHERE round_id IS NOT NULL;

-- Separate partial index covering OOR session-linked events so
-- VTXO send idempotency works off session_id without colliding
-- with the round-id index.
CREATE UNIQUE INDEX IF NOT EXISTS idx_client_ledger_idempotent_session
    ON ledger_entries(session_id, event_type, debit_account, credit_account)
    WHERE session_id IS NOT NULL;

-- Partial index covering outpoint-keyed events (unilateral exit
-- legs) so crash-replay of a durable actor message does not
-- double-book transfers_out and onchain_fees.
CREATE UNIQUE INDEX IF NOT EXISTS idx_client_ledger_idempotent_key
    ON ledger_entries(
        idempotency_key, event_type, debit_account, credit_account
    )
    WHERE idempotency_key IS NOT NULL;
